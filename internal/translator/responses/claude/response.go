package claude

import (
	"context"

	chattoclaude "github.com/Mieluoxxx/lite-cpa/internal/translator/chat/claude"
	respchat "github.com/Mieluoxxx/lite-cpa/internal/translator/responses/chat"
)

// hopState holds separate translator state for each hop:
// Responses→Chat and Chat→Claude. Do not merge into one param.
type hopState struct {
	Chat any
	Cla  any
}

// ConvertOpenAIResponsesResponseToClaude converts Responses SSE → Claude SSE
// by Responses→Chat then Chat→Claude, preserving both hop states.
func ConvertOpenAIResponsesResponseToClaude(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &hopState{}
	}
	st := (*param).(*hopState)
	chatChunks := respchat.ConvertOpenAIResponsesResponseToOpenAIChatCompletions(ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, &st.Chat)
	var out [][]byte
	for _, chunk := range chatChunks {
		parts := chattoclaude.ConvertOpenAIResponseToClaude(ctx, modelName, originalRequestRawJSON, requestRawJSON, chunk, &st.Cla)
		out = append(out, parts...)
	}
	return out
}

// ConvertOpenAIResponsesResponseToClaudeNonStream converts a full Responses object
// into a Claude non-stream message via Chat as an intermediate format.
func ConvertOpenAIResponsesResponseToClaudeNonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	_ = param
	chat := respchat.ConvertOpenAIResponsesResponseToOpenAIChatCompletionsNonStream(ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, new(any))
	return chattoclaude.ConvertOpenAIResponseToClaudeNonStream(ctx, modelName, originalRequestRawJSON, requestRawJSON, chat, new(any))
}
