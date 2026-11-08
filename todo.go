package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TodoItem is one line of the model's checklist. Status is pending, doing
// or done.
type TodoItem struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

const todoEvery = 15 // steps between reminders of the open items

func todoSchema() map[string]any {
	item := map[string]any{"type": "object", "properties": map[string]any{
		"text":   map[string]any{"type": "string"},
		"status": map[string]any{"type": "string", "enum": []string{"pending", "doing", "done"}},
	}, "required": []string{"text", "status"}}
	return map[string]any{"type": "function", "function": map[string]any{
		"name":        "todo",
		"description": "Write your checklist. Each call replaces the whole list.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"items": map[string]any{"type": "array", "items": item},
		}, "required": []string{"items"}},
	}}
}

// parseTodo reads the items argument. Besides the array of objects it takes
// the array as a JSON string (the XML tool call fallback gives strings) and
// plain strings, which count as pending.
func parseTodo(raw string) ([]TodoItem, error) {
	var args struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %v", err)
	}
	if len(args.Items) == 0 {
		return nil, fmt.Errorf(`todo needs the "items" argument`)
	}
	var s string
	if json.Unmarshal(args.Items, &s) == nil {
		args.Items = json.RawMessage(s)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(args.Items, &list); err != nil {
		return nil, fmt.Errorf(`items must be a list of {"text": "...", "status": "pending|doing|done"}`)
	}
	var out []TodoItem
	for _, r := range list {
		var it TodoItem
		if json.Unmarshal(r, &s) == nil {
			it.Text = s
		} else if err := json.Unmarshal(r, &it); err != nil {
			return nil, fmt.Errorf(`each item is {"text": "...", "status": "pending|doing|done"}`)
		}
		it.Text = strings.TrimSpace(it.Text)
		switch st := strings.ToLower(strings.TrimSpace(it.Status)); st {
		case "", "pending", "todo", "open":
			it.Status = "pending"
		case "doing", "in_progress", "in progress", "active":
			it.Status = "doing"
		case "done", "completed", "complete":
			it.Status = "done"
		default:
			return nil, fmt.Errorf("status %q is not pending, doing or done", it.Status)
		}
		if it.Text != "" {
			out = append(out, it)
		}
	}
	return out, nil
}

// runTodo replaces the session's list, emits one info line per change and
// returns the short result for the model.
func runTodo(s *Session, raw string, emit func(Event)) (string, bool) {
	items, err := parseTodo(raw)
	if err != nil {
		return "error: " + err.Error(), false
	}
	for _, l := range todoChanges(s.Todo, items) {
		emit(Event{T: "info", Text: "todo " + l})
	}
	s.Todo = items
	done := 0
	for _, it := range items {
		if it.Status == "done" {
			done++
		}
	}
	return fmt.Sprintf("%d items, %d done", len(items), done), true
}

func todoChanges(old, nw []TodoItem) []string {
	if len(old) == 0 {
		if len(nw) == 0 {
			return nil
		}
		var t []string
		for _, it := range nw {
			t = append(t, it.Text)
		}
		return []string{oneLine(fmt.Sprintf("%d items: %s", len(nw), strings.Join(t, "; ")), 200)}
	}
	was := map[string]string{}
	for _, it := range old {
		was[it.Text] = it.Status
	}
	var out []string
	keep := map[string]bool{}
	for _, it := range nw {
		keep[it.Text] = true
		st, ok := was[it.Text]
		switch {
		case !ok:
			out = append(out, "+ "+oneLine(it.Text, 160)+" ("+it.Status+")")
		case st != it.Status:
			out = append(out, it.Status+": "+oneLine(it.Text, 160))
		}
	}
	for _, it := range old {
		if !keep[it.Text] {
			out = append(out, "- "+oneLine(it.Text, 160))
		}
	}
	return out
}

// todoOpen lists the items that are not done, short form.
func todoOpen(items []TodoItem) []string {
	var out []string
	for _, it := range items {
		if it.Status != "done" {
			out = append(out, it.Status+": "+oneLine(it.Text, 160))
		}
	}
	return out
}

// todoReminder is the note for the end of a tool result, or "".
func todoReminder(items []TodoItem) string {
	o := todoOpen(items)
	if len(o) == 0 {
		return ""
	}
	return "\n\n[qh: open todo items: " + strings.Join(o, "; ") + "]"
}

// todoSelfCheck adds the open items to the self-check request.
func todoSelfCheck(items []TodoItem) string {
	o := todoOpen(items)
	if len(o) == 0 {
		return ""
	}
	return "\n\nThese todo items are not done yet: " + strings.Join(o, "; ") + ". Finish them or say why not, and update the list."
}
