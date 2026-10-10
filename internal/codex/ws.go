package codex

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	binenc "encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Минимальный клиент RFC 6455 — ровно то, на чём говорит управляющий сокет
// app-server: JSON-RPC текстовыми кадрами поверх WebSocket поверх unix-сокета.
// Своя реализация вместо зависимости: нужны только текстовые кадры, склейка
// фрагментов, ответ на ping и закрытие.

const (
	opCont  = 0x0
	opText  = 0x1
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxMessage — предел на одно сообщение. Сервер объявляет 16 МиБ; больше
// claudex не нужно, а без предела битый кадр заставит выделить гигабайты.
const maxMessage = 16 << 20

type wsConn struct {
	c net.Conn
	r *bufio.Reader
}

func wsHandshake(c net.Conn) (*wsConn, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req := "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return nil, err
	}
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		return nil, fmt.Errorf("рукопожатие WebSocket: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("рукопожатие WebSocket: сервер ответил %s", resp.Status)
	}
	if got := resp.Header.Get("Sec-Websocket-Accept"); got != acceptKey(key) {
		return nil, errors.New("рукопожатие WebSocket: неверный Sec-WebSocket-Accept")
	}
	return &wsConn{c: c, r: r}, nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// AcceptKey нужен поддельному серверу в тестах.
func AcceptKey(key string) string { return acceptKey(key) }

func (w *wsConn) writeFrame(op byte, payload []byte) error {
	return WriteFrame(w.c, op, payload, true)
}

// WriteFrame пишет один кадр. Клиент обязан маскировать, сервер — нет.
func WriteFrame(c io.Writer, op byte, payload []byte, mask bool) error {
	hdr := []byte{0x80 | op}
	n := len(payload)
	var mbit byte
	if mask {
		mbit = 0x80
	}
	switch {
	case n < 126:
		hdr = append(hdr, mbit|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, mbit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, mbit|127)
		hdr = binenc.BigEndian.AppendUint64(hdr, uint64(n))
	}
	body := payload
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		hdr = append(hdr, key[:]...)
		body = make([]byte, n)
		for i := range payload {
			body[i] = payload[i] ^ key[i%4]
		}
	}
	_, err := c.Write(append(hdr, body...))
	return err
}

// ReadFrame читает один кадр, снимая маску, если она есть.
func ReadFrame(r io.Reader) (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		n = uint64(binenc.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		n = binenc.BigEndian.Uint64(b[:])
	}
	if n > maxMessage {
		err = fmt.Errorf("кадр WebSocket в %d байт больше предела", n)
		return
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return
}

// readMessage возвращает следующее текстовое сообщение целиком.
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	inText := false
	for {
		fin, op, p, err := ReadFrame(w.r)
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := w.writeFrame(opPong, p); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = w.writeFrame(opClose, nil)
			return nil, io.EOF
		case opText:
			msg, inText = append(msg[:0], p...), true
		case opCont:
			if !inText {
				return nil, errors.New("кадр-продолжение без начала сообщения")
			}
			msg = append(msg, p...)
		default:
			return nil, fmt.Errorf("неожиданный кадр WebSocket 0x%x", op)
		}
		if len(msg) > maxMessage {
			return nil, errors.New("сообщение WebSocket больше предела")
		}
		if fin {
			return msg, nil
		}
	}
}

func (w *wsConn) close() error {
	_ = w.writeFrame(opClose, nil)
	return w.c.Close()
}

// IsUpgrade — просит ли запрос WebSocket. Нужен поддельному серверу.
func IsUpgrade(h http.Header) bool {
	return strings.EqualFold(h.Get("Upgrade"), "websocket")
}
