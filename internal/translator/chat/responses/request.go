package responses

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIChatCompletionsRequestToOpenAIResponses converts a standard
// OpenAI Chat Completions request into a standard OpenAI Responses API request
// suitable for POST /v1/responses (not Codex-specific).
func ConvertOpenAIChatCompletionsRequestToOpenAIResponses(modelName string, inputRawJSON []byte, stream bool) []byte {
	root := gjson.ParseBytes(inputRawJSON)
	out := []byte(`{"model":"","input":[],"stream":false}`)
	out, _ = sjson.SetBytes(out, "model", modelName)
	out, _ = sjson.SetBytes(out, "stream", stream)

	if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_output_tokens", maxTokens.Int())
	}
	if maxTokens := root.Get("max_completion_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_output_tokens", maxTokens.Int())
	}
	if temp := root.Get("temperature"); temp.Exists() && temp.Type != gjson.Null {
		out, _ = sjson.SetBytes(out, "temperature", temp.Float())
	}
	if topP := root.Get("top_p"); topP.Exists() && topP.Type != gjson.Null {
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}
	if ptc := root.Get("parallel_tool_calls"); ptc.Exists() {
		out, _ = sjson.SetBytes(out, "parallel_tool_calls", ptc.Bool())
	}
	if effort := root.Get("reasoning_effort"); effort.Exists() {
		out, _ = sjson.SetBytes(out, "reasoning.effort", effort.String())
	}

	// messages → instructions + input
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		for _, msg := range messages.Array() {
			role := msg.Get("role").String()
			switch role {
			case "system", "developer":
				// Fold system/developer into instructions (append).
				text := extractChatText(msg.Get("content"))
				if text == "" {
					continue
				}
				if existing := gjson.GetBytes(out, "instructions"); existing.Exists() && existing.String() != "" {
					out, _ = sjson.SetBytes(out, "instructions", existing.String()+"\n\n"+text)
				} else {
					out, _ = sjson.SetBytes(out, "instructions", text)
				}
			case "user", "assistant":
				item := []byte(`{"type":"message","role":"","content":[]}`)
				item, _ = sjson.SetBytes(item, "role", role)
				content := msg.Get("content")
				if content.Type == gjson.String {
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := []byte(`{"type":"","text":""}`)
					part, _ = sjson.SetBytes(part, "type", partType)
					part, _ = sjson.SetBytes(part, "text", content.String())
					item, _ = sjson.SetRawBytes(item, "content.-1", part)
				} else if content.IsArray() {
					for _, part := range content.Array() {
						switch part.Get("type").String() {
						case "text", "":
							partType := "input_text"
							if role == "assistant" {
								partType = "output_text"
							}
							p := []byte(`{"type":"","text":""}`)
							p, _ = sjson.SetBytes(p, "type", partType)
							p, _ = sjson.SetBytes(p, "text", part.Get("text").String())
							item, _ = sjson.SetRawBytes(item, "content.-1", p)
						case "image_url":
							p := []byte(`{"type":"input_image","image_url":""}`)
							url := part.Get("image_url.url").String()
							if url == "" {
								url = part.Get("image_url").String()
							}
							p, _ = sjson.SetBytes(p, "image_url", url)
							item, _ = sjson.SetRawBytes(item, "content.-1", p)
						}
					}
				}
				// Assistant tool_calls → function_call items (after message if any text)
				if role == "assistant" {
					if tools := msg.Get("tool_calls"); tools.Exists() && tools.IsArray() {
						// Emit text message first if content present
						if len(gjson.GetBytes(item, "content").Array()) > 0 {
							out, _ = sjson.SetRawBytes(out, "input.-1", item)
						}
						for _, tc := range tools.Array() {
							fc := []byte(`{"type":"function_call","call_id":"","name":"","arguments":""}`)
							fc, _ = sjson.SetBytes(fc, "call_id", tc.Get("id").String())
							fc, _ = sjson.SetBytes(fc, "name", tc.Get("function.name").String())
							fc, _ = sjson.SetBytes(fc, "arguments", tc.Get("function.arguments").String())
							out, _ = sjson.SetRawBytes(out, "input.-1", fc)
						}
						continue
					}
				}
				out, _ = sjson.SetRawBytes(out, "input.-1", item)
			case "tool":
				fcOut := []byte(`{"type":"function_call_output","call_id":"","output":""}`)
				fcOut, _ = sjson.SetBytes(fcOut, "call_id", msg.Get("tool_call_id").String())
				fcOut, _ = sjson.SetBytes(fcOut, "output", extractChatText(msg.Get("content")))
				out, _ = sjson.SetRawBytes(out, "input.-1", fcOut)
			}
		}
	}

	// tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		for _, tool := range tools.Array() {
			// chat: {type:function, function:{name, description, parameters}}
			// responses: {type:function, name, description, parameters}
			t := []byte(`{"type":"function","name":"","parameters":{}}`)
			name := tool.Get("function.name").String()
			if name == "" {
				name = tool.Get("name").String()
			}
			desc := tool.Get("function.description").String()
			if desc == "" {
				desc = tool.Get("description").String()
			}
			params := tool.Get("function.parameters").Raw
			if params == "" {
				params = tool.Get("parameters").Raw
			}
			t, _ = sjson.SetBytes(t, "name", name)
			if desc != "" {
				t, _ = sjson.SetBytes(t, "description", desc)
			}
			if params != "" {
				t, _ = sjson.SetRawBytes(t, "parameters", []byte(params))
			}
			out, _ = sjson.SetRawBytes(out, "tools.-1", t)
		}
	}

	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		// Map common chat tool_choice forms.
		switch toolChoice.Type {
		case gjson.String:
			out, _ = sjson.SetBytes(out, "tool_choice", toolChoice.String())
		default:
			if toolChoice.Get("type").String() == "function" {
				// chat: {type:function, function:{name}}
				// responses accepts similar or string
				name := toolChoice.Get("function.name").String()
				tc := []byte(`{"type":"function","name":""}`)
				tc, _ = sjson.SetBytes(tc, "name", name)
				out, _ = sjson.SetRawBytes(out, "tool_choice", tc)
			} else {
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(toolChoice.Raw))
			}
		}
	}

	return out
}

func extractChatText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var b string
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text", "input_text", "output_text", "":
				b += part.Get("text").String()
			}
		}
		return b
	}
	return ""
}
