package server

import (
	"bytes"

	"github.com/tidwall/gjson"
)

// tokenUsage records the last reported input and output token totals for one request.
// A response may report input and output usage in separate stream events.
type tokenUsage struct {
	inputTokens  int64
	outputTokens int64
	cachedTokens int64
	inputSeen    bool
	outputSeen   bool
	cachedSeen   bool
}

func (u tokenUsage) complete() bool {
	return u.inputSeen && u.outputSeen
}

func usageFromResponse(payload []byte) tokenUsage {
	var usage tokenUsage
	usage.mergePayload(payload)
	return usage
}

func (u *tokenUsage) mergePayload(payload []byte) {
	seenData := false
	for _, line := range bytes.Split(payload, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		seenData = true
		u.mergeJSON(bytes.TrimSpace(line[len("data:"):]))
	}
	if !seenData {
		u.mergeJSON(bytes.TrimSpace(payload))
	}
}

func (u *tokenUsage) mergeJSON(payload []byte) {
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	for _, usage := range []gjson.Result{
		root.Get("usage"),
		root.Get("response.usage"),
		root.Get("message.usage"),
	} {
		u.mergeUsage(usage)
	}
}

func (u *tokenUsage) mergeUsage(usage gjson.Result) {
	if !usage.Exists() {
		return
	}

	if prompt := usage.Get("prompt_tokens"); prompt.Exists() {
		u.inputTokens = prompt.Int()
		u.inputSeen = true
	} else if input := usage.Get("input_tokens"); input.Exists() {
		u.inputTokens = input.Int() + usage.Get("cache_creation_input_tokens").Int() + usage.Get("cache_read_input_tokens").Int()
		u.inputSeen = true
	}

	if cached := usage.Get("prompt_tokens_details.cached_tokens"); cached.Exists() {
		u.cachedTokens = cached.Int()
		u.cachedSeen = true
	} else if cached := usage.Get("input_tokens_details.cached_tokens"); cached.Exists() {
		u.cachedTokens = cached.Int()
		u.cachedSeen = true
	} else if cacheRead := usage.Get("cache_read_input_tokens"); cacheRead.Exists() {
		u.cachedTokens = cacheRead.Int()
		u.cachedSeen = true
	}

	if completion := usage.Get("completion_tokens"); completion.Exists() {
		u.outputTokens = completion.Int()
		u.outputSeen = true
	} else if output := usage.Get("output_tokens"); output.Exists() {
		u.outputTokens = output.Int()
		u.outputSeen = true
	}
}
