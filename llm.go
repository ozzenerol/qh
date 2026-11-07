package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	Details          struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type LLM struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

type Request struct {
	Messages []Message
	Tools    []map[string]any
	Think    bool
	MaxTok   int
}

type Delta struct {
	Reasoning string
	Text      string
}

// errStopped is returned when the onDelta callback asks to end the stream.
var errStopped = fmt.Errorf("stream stopped")

func (l *LLM) ResolveModel(ctx context.Context) error {
	if l.Model != "" {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", l.BaseURL+"/models", nil)
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("model server at %s: %w", l.BaseURL, err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Data) == 0 {
		return fmt.Errorf("no model at %s/models", l.BaseURL)
	}
	// mlx_lm.server lists every cached model; the loaded one is a local path.
	for _, m := range out.Data {
		if strings.HasPrefix(m.ID, "/") {
			l.Model = m.ID
			return nil
		}
	}
	l.Model = out.Data[0].ID
	return nil
}

// Chat streams one completion. onDelta returns false to stop the stream.
func (l *LLM) Chat(ctx context.Context, r Request, onDelta func(Delta) bool) (Message, Usage, error) {
	body := map[string]any{
		"model":          l.Model,
		"messages":       r.Messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"max_tokens":     r.MaxTok,
		// preserve_thinking keeps old reasoning in the rendered prompt, so each
		// request is an exact extension of the cached one.
		"chat_template_kwargs": map[string]any{"enable_thinking": r.Think, "preserve_thinking": true},
	}
	if len(r.Tools) > 0 {
		body["tools"] = r.Tools
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", l.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return Message{}, Usage{}, fmt.Errorf("model server: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	msg := Message{Role: "assistant"}
	var usage Usage
	var reasoning, text strings.Builder
	calls := map[int]*ToolCall{}
	order := []int{}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, c := range chunk.Choices {
			d := Delta{Text: c.Delta.Content, Reasoning: c.Delta.ReasoningContent}
			if d.Reasoning == "" {
				d.Reasoning = c.Delta.Reasoning
			}
			reasoning.WriteString(d.Reasoning)
			text.WriteString(d.Text)
			for _, tc := range c.Delta.ToolCalls {
				cur, ok := calls[tc.Index]
				if !ok {
					cur = &ToolCall{Type: "function"}
					calls[tc.Index] = cur
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				cur.Function.Name += tc.Function.Name
				cur.Function.Arguments += tc.Function.Arguments
			}
			if (d.Text != "" || d.Reasoning != "") && !onDelta(d) {
				msg.ReasoningContent = strings.TrimSpace(reasoning.String())
				msg.Content = text.String()
				return msg, usage, errStopped
			}
		}
	}
	if err := sc.Err(); err != nil {
		return msg, usage, err
	}
	msg.ReasoningContent = strings.TrimSpace(reasoning.String())
	msg.Content = text.String()
	for i, idx := range order {
		tc := *calls[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
		}
		msg.ToolCalls = append(msg.ToolCalls, tc)
	}
	if len(msg.ToolCalls) == 0 {
		msg.Content, msg.ToolCalls = parseTextToolCalls(msg.Content)
	}
	return msg, usage, nil
}

// parseTextToolCalls finds Qwen XML tool calls that the server left in the text.
func parseTextToolCalls(s string) (string, []ToolCall) {
	i := strings.Index(s, "<tool_call>")
	if i < 0 {
		return s, nil
	}
	rest := s[i:]
	var calls []ToolCall
	for {
		a := strings.Index(rest, "<function=")
		if a < 0 {
			break
		}
		rest = rest[a+len("<function="):]
		e := strings.Index(rest, ">")
		if e < 0 {
			break
		}
		name := strings.TrimSpace(rest[:e])
		rest = rest[e+1:]
		end := strings.Index(rest, "</function>")
		body := rest
		if end >= 0 {
			body = rest[:end]
			rest = rest[end:]
		} else {
			rest = ""
		}
		args := map[string]any{}
		for {
			p := strings.Index(body, "<parameter=")
			if p < 0 {
				break
			}
			body = body[p+len("<parameter="):]
			pe := strings.Index(body, ">")
			if pe < 0 {
				break
			}
			key := strings.TrimSpace(body[:pe])
			body = body[pe+1:]
			ve := strings.Index(body, "</parameter>")
			val := body
			if ve >= 0 {
				val = body[:ve]
				body = body[ve+len("</parameter>"):]
			} else {
				body = ""
			}
			val = strings.TrimPrefix(val, "\n")
			val = strings.TrimSuffix(val, "\n")
			var num json.Number
			if json.Unmarshal([]byte(val), &num) == nil {
				args[key] = num
			} else {
				args[key] = val
			}
		}
		b, _ := json.Marshal(args)
		tc := ToolCall{ID: fmt.Sprintf("call_t%d_%d", time.Now().UnixNano(), len(calls)), Type: "function"}
		tc.Function.Name = name
		tc.Function.Arguments = string(b)
		calls = append(calls, tc)
	}
	if len(calls) == 0 {
		return s, nil
	}
	return strings.TrimSpace(s[:i]), calls
}
