// Package cass — семантический и гибридный поиск через официальный интерфейс
// cass.
//
// Ходим только в CLI `cass search --json`: ни в SQLite напрямую, ни в сырое
// зеркало. Схема у cass своя и меняется, а контракт JSON объявлен и содержит
// то, ради чего всё затевается — каким режимом искали на самом деле, была ли
// семантическая доводка и насколько свеж индекс.
//
// Тихого ухудшения быть не должно. cass умеет молча откатиться на лексический
// поиск, и без разбора _meta это выглядит как удачный семантический запрос;
// поэтому причина отката, факт таймаута и расхождение запрошенного режима с
// фактическим поднимаются наверх и показываются.
package cass

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	ModeHybrid   = "hybrid"
	ModeSemantic = "semantic"
	ModeLexical  = "lexical"
)

// DefaultTimeout — предел одного запроса.
//
// Замерено на живом архиве 16.09, 4054 разговора: по часам 20–22 с, при
// собственном elapsed_ms у cass 10–12.5 с. Разница — запуск процесса и загрузка
// модели; с поднятым `cass daemon` она уходит, и запрос укладывается примерно
// вдвое быстрее. Предел взят по худшему из замеренных, с запасом: срабатывающий
// на исправной машине откат хуже медленного ответа.
//
// Собственный предел cass — две минуты, и для команды, которую ждёт человек,
// это не предел вовсе.
const DefaultTimeout = 30 * time.Second

type Options struct {
	Mode        string
	Limit       int
	Days        int
	Timeout     time.Duration
	Approximate bool
	Workspace   string
	Agent       string
}

type Hit struct {
	SourcePath string  `json:"source_path"`
	Session    string  `json:"session,omitempty"`
	Line       int     `json:"line,omitempty"`
	TS         int64   `json:"ts,omitempty"`
	Score      float64 `json:"score"`
	MatchType  string  `json:"match_type,omitempty"`
	Snippet    string  `json:"snippet,omitempty"`
	Title      string  `json:"title,omitempty"`
	Workspace  string  `json:"workspace,omitempty"`
	Agent      string  `json:"agent,omitempty"`
}

// Meta — то, что cass сообщает о самом поиске. Ради него интеграция и нужна:
// без этих полей отличить настоящую семантику от молчаливого отката нечем.
type Meta struct {
	SearchMode         string `json:"search_mode,omitempty"`
	RequestedMode      string `json:"requested_search_mode,omitempty"`
	SemanticRefinement bool   `json:"semantic_refinement"`
	RefinementLevel    string `json:"refinement_level,omitempty"`

	FallbackTier           string `json:"fallback_tier,omitempty"`
	FallbackReason         string `json:"fallback_reason,omitempty"`
	SemanticFallbackReason string `json:"semantic_fallback_reason,omitempty"`

	TimedOut  bool `json:"timed_out,omitempty"`
	ElapsedMS int  `json:"elapsed_ms,omitempty"`

	Fresh         bool   `json:"fresh"`
	Stale         bool   `json:"stale,omitempty"`
	LastIndexedAt string `json:"last_indexed_at,omitempty"`
	AgeSeconds    int    `json:"age_seconds,omitempty"`
	Rebuilding    bool   `json:"rebuilding,omitempty"`
	Partial       bool   `json:"partial,omitempty"`
	IndexStatus   string `json:"index_status,omitempty"`
}

// Degraded — почему выдаче нельзя верить как семантической. Пусто значит
// «искали тем, чем просили».
func (m Meta) Degraded(requested string) string {
	switch {
	case m.TimedOut:
		return fmt.Sprintf("cass не уложился в свой предел (%d мс)", m.ElapsedMS)
	case m.SemanticFallbackReason != "":
		return "семантика отключилась: " + m.SemanticFallbackReason
	case m.FallbackReason != "":
		return "cass откатился: " + m.FallbackReason
	case requested == ModeSemantic && m.SearchMode != ModeSemantic,
		requested == ModeHybrid && m.SearchMode == ModeLexical:
		return fmt.Sprintf("просили %s, искали %s", requested, m.SearchMode)
	}
	return ""
}

// StaleNote — что не так со свежестью индекса. Пусто значит «свеж».
func (m Meta) StaleNote() string {
	switch {
	case m.Rebuilding:
		return "индекс cass перестраивается: выдача неполна"
	case m.Partial:
		return "индекс cass построен частично"
	case m.Stale || !m.Fresh:
		return fmt.Sprintf("индекс cass отстал на %d с (последняя сборка %s)",
			m.AgeSeconds, m.LastIndexedAt)
	}
	return ""
}

type Result struct {
	Hits []Hit `json:"hits"`
	Meta Meta  `json:"meta"`
}

// Run — вызов бинаря. Подменяется в тестах: настоящий ходит в живой архив и
// стоит секунды, а проверять им разбор контракта незачем.
var Run = func(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary(), args...)
	out, err := cmd.Output()
	if err != nil {
		return out, err
	}
	return out, nil
}

func binary() string {
	if p := strings.TrimSpace(os.Getenv("CLAUDEX_CASS_BIN")); p != "" {
		return p
	}
	return "cass"
}

// Search спрашивает cass и разбирает объявленный контракт.
func Search(ctx context.Context, query string, o Options) (Result, error) {
	if strings.TrimSpace(query) == "" {
		return Result{}, fmt.Errorf("пустой запрос")
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Mode == "" {
		o.Mode = ModeHybrid
	}

	args := []string{"search", query, "--json", "--robot-meta", "--mode", o.Mode}
	if o.Limit > 0 {
		args = append(args, "--limit", itoa(o.Limit))
	}
	if o.Days > 0 {
		args = append(args, "--days", itoa(o.Days))
	}
	if o.Approximate {
		args = append(args, "--approximate")
	}
	if o.Workspace != "" {
		args = append(args, "--workspace", o.Workspace)
	}
	if o.Agent != "" {
		args = append(args, "--agent", o.Agent)
	}

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	out, err := Run(ctx, args)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("cass не ответил за %s", o.Timeout)
		}
		return Result{}, fmt.Errorf("cass search: %w", err)
	}
	return parse(out)
}

func parse(out []byte) (Result, error) {
	var raw struct {
		Hits []struct {
			SourcePath string  `json:"source_path"`
			SourceID   string  `json:"source_id"`
			Line       int     `json:"line_number"`
			CreatedAt  int64   `json:"created_at"`
			Score      float64 `json:"score"`
			MatchType  string  `json:"match_type"`
			Snippet    string  `json:"snippet"`
			Content    string  `json:"content"`
			Title      string  `json:"title"`
			Workspace  string  `json:"workspace"`
			Agent      string  `json:"agent"`
		} `json:"hits"`
		Meta struct {
			SearchMode             string `json:"search_mode"`
			RequestedMode          string `json:"requested_search_mode"`
			SemanticRefinement     bool   `json:"semantic_refinement"`
			RefinementLevel        string `json:"refinement_level"`
			FallbackTier           string `json:"fallback_tier"`
			FallbackReason         string `json:"fallback_reason"`
			SemanticFallbackReason string `json:"semantic_fallback_reason"`
			TimedOut               bool   `json:"timed_out"`
			ElapsedMS              int    `json:"elapsed_ms"`
			Freshness              struct {
				Status        string `json:"status"`
				Fresh         bool   `json:"fresh"`
				Stale         bool   `json:"stale"`
				LastIndexedAt string `json:"last_indexed_at"`
				AgeSeconds    int    `json:"age_seconds"`
				Rebuilding    bool   `json:"rebuilding"`
				Partial       bool   `json:"partial"`
			} `json:"index_freshness"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Result{}, fmt.Errorf("ответ cass не разобрался: %w", err)
	}

	r := Result{Meta: Meta{
		SearchMode: raw.Meta.SearchMode, RequestedMode: raw.Meta.RequestedMode,
		SemanticRefinement:     raw.Meta.SemanticRefinement,
		RefinementLevel:        raw.Meta.RefinementLevel,
		FallbackTier:           raw.Meta.FallbackTier,
		FallbackReason:         raw.Meta.FallbackReason,
		SemanticFallbackReason: raw.Meta.SemanticFallbackReason,
		TimedOut:               raw.Meta.TimedOut,
		ElapsedMS:              raw.Meta.ElapsedMS,
		Fresh:                  raw.Meta.Freshness.Fresh,
		Stale:                  raw.Meta.Freshness.Stale,
		LastIndexedAt:          raw.Meta.Freshness.LastIndexedAt,
		AgeSeconds:             raw.Meta.Freshness.AgeSeconds,
		Rebuilding:             raw.Meta.Freshness.Rebuilding,
		Partial:                raw.Meta.Freshness.Partial,
		IndexStatus:            raw.Meta.Freshness.Status,
	}}
	for _, h := range raw.Hits {
		text := h.Snippet
		if text == "" {
			text = h.Content
		}
		r.Hits = append(r.Hits, Hit{
			SourcePath: h.SourcePath, Session: SessionOf(h.SourcePath),
			Line: h.Line, TS: h.CreatedAt, Score: h.Score,
			MatchType: h.MatchType, Snippet: text, Title: h.Title,
			Workspace: h.Workspace, Agent: h.Agent,
		})
	}
	return r, nil
}

// SessionOf вытаскивает идентификатор разговора из пути транскрипта.
//
// У Claude имя файла — сам идентификатор, у Codex — rollout-<метка>-<uuid>,
// поэтому берётся хвост из 36 знаков. Это единственный общий ключ между выдачей
// cass и локальным индексом: сводить их по тексту нельзя, cass отдаёт фрагмент
// с подсветкой, а не исходную запись.
func SessionOf(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(base) < 36 {
		return base
	}
	tail := base[len(base)-36:]
	if isUUID(tail) {
		return tail
	}
	return base
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
