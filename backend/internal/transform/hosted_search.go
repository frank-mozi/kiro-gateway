package transform

import (
	"errors"
	"maps"
	"net/url"
	"strings"

	"github.com/mydisha/keirouter/backend/internal/core"
	json "github.com/mydisha/keirouter/backend/internal/fastjson"
)

type searchObject = map[string]any

// This encoder consumes a completed receipt, not model-generated tool arguments.
// It never looks up a URL, invents a citation, or synthesizes replay tokens.
func encodeHostedSearch(d core.Dialect, r *core.ChatResponse, stream bool) (searchObject, [][]byte, error) {
	if r == nil || r.HostedSearch == nil || r.ID == "" || r.Model == "" || r.FinishReason != core.FinishStop {
		return nil, nil, errors.New("hosted search requires a completed model response")
	}
	if d != core.DialectAnthropic && d != core.DialectOpenAIResponses {
		return nil, nil, errors.New("unsupported hosted search response dialect")
	}
	if r.Message.Role != core.RoleAssistant || len(r.Message.Content) != 1 || r.Message.Content[0].Type != core.PartText || r.Message.Content[0].Text == "" {
		return nil, nil, errors.New("expected one final text part for a completed search")
	}
	h := r.HostedSearch
	if h.ID == "" || strings.TrimSpace(h.Query) == "" {
		return nil, nil, errors.New("search receipt is missing its call id or query")
	}
	if d == core.DialectOpenAIResponses {
		if h.CreatedAt <= 0 || h.Request == nil || len(h.Request.Tools) == 0 {
			return nil, nil, errors.New("Responses search receipt lacks original tool options or creation time")
		}
		for _, raw := range h.Request.Tools {
			if _, hosted := hostedSearchTool(raw); !hosted {
				return nil, nil, errors.New("invalid Responses search tool declaration")
			}
		}
		if choice := h.Request.ToolChoice; len(choice) != 0 && !json.Valid(choice) {
			return nil, nil, errors.New("invalid Responses tool choice")
		}
	}
	for _, s := range h.Sources {
		u, err := url.Parse(s.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || s.Title == "" {
			return nil, nil, errors.New("invalid source in search receipt")
		}
		if d == core.DialectAnthropic && s.EncryptedContent == "" {
			return nil, nil, errors.New("search source lacks opaque Anthropic replay content")
		}
	}
	text := []rune(r.Message.Content[0].Text)
	end := 0
	for _, c := range h.Citations {
		if c.SourceIndex < 0 || c.SourceIndex >= len(h.Sources) || c.Start < end || c.End <= c.Start || c.End > len(text) {
			return nil, nil, errors.New("invalid search citation source or span")
		}
		if d == core.DialectAnthropic && (c.CitedText == "" || c.EncryptedIndex == "") {
			return nil, nil, errors.New("search citation lacks cited text or an opaque replay index")
		}
		end = c.End
	}
	var frames [][]byte
	emit := func(name string, obj searchObject) {
		if !stream {
			return
		}
		obj["type"] = name
		if d == core.DialectOpenAIResponses {
			obj["sequence_number"] = len(frames) + 1
		}
		frames = append(frames, sseNamed(name, obj))
	}
	var body searchObject
	if d == core.DialectAnthropic {
		body = anthropicSearchReply(r, text, emit)
	} else {
		body = responsesSearchReply(r, emit)
	}
	return body, frames, nil
}

func renderHostedSearchResponse(d core.Dialect, r *core.ChatResponse) ([]byte, error) {
	obj, _, err := encodeHostedSearch(d, r, false)
	if err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}

// RenderHostedSearchStream serializes the same buffered, completed turn as
// native SSE. Validation happens before the first frame is returned.
func RenderHostedSearchStream(d core.Dialect, r *core.ChatResponse) ([][]byte, error) {
	_, frames, err := encodeHostedSearch(d, r, true)
	return frames, err
}

func anthropicSearchReply(r *core.ChatResponse, text []rune, emit func(string, searchObject)) searchObject {
	h := r.HostedSearch
	id := "srvtoolu_" + h.ID
	usage := make(searchObject)
	for k, v := range renderAntUsage(r.Usage) {
		usage[k] = v
	}
	if r.Usage.WebSearchRequests > 0 {
		usage["server_tool_use"] = searchObject{"web_search_requests": r.Usage.WebSearchRequests}
	}
	start := searchObject{"id": r.ID, "type": "message", "role": "assistant", "model": r.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil}
	startUsage := maps.Clone(usage)
	delete(startUsage, "server_tool_use")
	startUsage["output_tokens"] = 0
	start["usage"] = startUsage
	emit("message_start", searchObject{"message": start})
	var content []searchObject
	block := func(full, initial searchObject, deltas ...searchObject) {
		idx := len(content)
		content = append(content, full)
		emit("content_block_start", searchObject{"index": idx, "content_block": initial})
		for _, delta := range deltas {
			emit("content_block_delta", searchObject{"index": idx, "delta": delta})
		}
		emit("content_block_stop", searchObject{"index": idx})
	}
	call := searchObject{"type": "server_tool_use", "id": id, "name": "web_search", "input": searchObject{"query": h.Query}}
	initial := maps.Clone(call)
	initial["input"] = searchObject{}
	query, _ := json.Marshal(map[string]string{"query": h.Query})
	block(call, initial, searchObject{"type": "input_json_delta", "partial_json": string(query)})
	sources := make([]searchObject, 0, len(h.Sources))
	for _, s := range h.Sources {
		sources = append(sources, searchObject{"type": "web_search_result", "url": s.URL, "title": s.Title, "encrypted_content": s.EncryptedContent})
	}
	result := searchObject{"type": "web_search_tool_result", "tool_use_id": id, "content": sources}
	block(result, result)
	addText := func(str string, citation searchObject) {
		full := searchObject{"type": "text", "text": str}
		deltas := []searchObject{{"type": "text_delta", "text": str}}
		if citation != nil {
			full["citations"] = []searchObject{citation}
			deltas = append(deltas, searchObject{"type": "citations_delta", "citation": citation})
		}
		block(full, searchObject{"type": "text", "text": ""}, deltas...)
	}
	end := 0
	for _, c := range h.Citations {
		s := h.Sources[c.SourceIndex]
		if end < c.Start {
			addText(string(text[end:c.Start]), nil)
		}
		addText(string(text[c.Start:c.End]), searchObject{"type": "web_search_result_location", "url": s.URL, "title": s.Title, "cited_text": c.CitedText, "encrypted_index": c.EncryptedIndex})
		end = c.End
	}
	if end < len(text) {
		addText(string(text[end:]), nil)
	}
	emit("message_delta", searchObject{"delta": searchObject{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": usage})
	emit("message_stop", searchObject{})
	done := maps.Clone(start)
	done["content"], done["stop_reason"], done["usage"] = content, "end_turn", usage
	return done
}

func responsesSearchReply(r *core.ChatResponse, emit func(string, searchObject)) searchObject {
	h := r.HostedSearch
	id, msgID := "ws_"+h.ID, "msg_"+r.ID
	body := searchObject{"object": "response", "id": r.ID, "model": r.Model, "status": "in_progress", "output": []any{}}
	body["created_at"], body["tools"], body["tool_choice"] = h.CreatedAt, h.Request.Tools, "auto"
	if len(h.Request.ToolChoice) != 0 {
		body["tool_choice"] = h.Request.ToolChoice
	}
	body["parallel_tool_calls"] = h.Request.ParallelToolCalls == nil || *h.Request.ParallelToolCalls
	emit("response.created", searchObject{"response": body})
	emit("response.in_progress", searchObject{"response": body})
	action := searchObject{"type": "search", "query": h.Query}
	call := searchObject{"type": "web_search_call", "id": id, "status": "in_progress", "action": action}
	emit("response.output_item.added", searchObject{"item": call, "output_index": 0})
	for _, name := range []string{"in_progress", "searching", "completed"} {
		emit("response.web_search_call."+name, searchObject{"item_id": id, "output_index": 0})
	}
	if h.Request.IncludeSources {
		sources := make([]searchObject, 0, len(h.Sources))
		for _, s := range h.Sources {
			sources = append(sources, searchObject{"type": "url", "url": s.URL})
		}
		action["sources"] = sources
	}
	call["status"] = "completed"
	emit("response.output_item.done", searchObject{"item": call, "output_index": 0})
	msg := searchObject{"type": "message", "role": "assistant", "id": msgID, "status": "in_progress", "content": []any{}}
	emit("response.output_item.added", searchObject{"item": msg, "output_index": 1})
	part := searchObject{"type": "output_text", "text": "", "annotations": []any{}}
	msgEvent := func(name string, fields searchObject) {
		fields["item_id"], fields["output_index"], fields["content_index"] = msgID, 1, 0
		emit("response."+name, fields)
	}
	msgEvent("content_part.added", searchObject{"part": part})
	text := r.Message.Content[0].Text
	msgEvent("output_text.delta", searchObject{"delta": text})
	annotations := make([]searchObject, 0, len(h.Citations))
	for i, c := range h.Citations {
		s := h.Sources[c.SourceIndex]
		a := searchObject{"type": "url_citation", "url": s.URL, "title": s.Title, "start_index": c.Start, "end_index": c.End}
		annotations = append(annotations, a)
		msgEvent("output_text.annotation.added", searchObject{"annotation_index": i, "annotation": a})
	}
	part["text"], part["annotations"] = text, annotations
	msgEvent("output_text.done", searchObject{"text": text})
	msgEvent("content_part.done", searchObject{"part": part})
	msg["status"], msg["content"] = "completed", []searchObject{part}
	emit("response.output_item.done", searchObject{"item": msg, "output_index": 1})
	body["status"], body["output"] = "completed", []searchObject{call, msg}
	body["usage"] = searchObject{"input_tokens": r.Usage.PromptTokens, "output_tokens": r.Usage.CompletionTokens, "total_tokens": r.Usage.TotalTokens}
	emit("response.completed", searchObject{"response": body})
	return body
}
