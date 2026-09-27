// Package llm wraps the official github.com/openai/openai-go client for
// streaming chat completions with tool calling.
package llm

import (
	"context"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"c2agent/internal/command"
)

// Message is the message union used in a chat request.
type Message = openai.ChatCompletionMessageParamUnion

// SystemMessage builds a system message.
func SystemMessage(s string) Message { return openai.SystemMessage(s) }

// UserMessage builds a user message.
func UserMessage(s string) Message { return openai.UserMessage(s) }

// ToolMessage builds a tool-result message.
func ToolMessage(content, toolCallID string) Message { return openai.ToolMessage(content, toolCallID) }

// ToolCall is one model-requested function call.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// AssistantMessage builds an assistant message, optionally with tool calls.
func AssistantMessage(content string, calls []ToolCall) Message {
	asst := &openai.ChatCompletionAssistantMessageParam{}
	if content != "" {
		asst.Content = openai.ChatCompletionAssistantMessageParamContentUnion{OfString: openai.String(content)}
	}
	for _, tc := range calls {
		asst.ToolCalls = append(asst.ToolCalls, openai.ChatCompletionMessageToolCallParam{
			ID: tc.ID,
			Function: openai.ChatCompletionMessageToolCallFunctionParam{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		})
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: asst}
}

// ChatResult is one assistant turn.
type ChatResult struct {
	Content   string
	ToolCalls []ToolCall
	Raw       Message // assistant message to append verbatim to history
}

// Client wraps the OpenAI-compatible chat completions API.
type Client struct {
	api         *openai.Client
	model       string
	temperature float64
	stream      bool
}

// New creates a client for an OpenAI-compatible endpoint.
func New(apiBase, apiKey, model string, temperature float64, stream bool) *Client {
	c := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(apiBase),
	)
	return &Client{api: &c, model: model, temperature: temperature, stream: stream}
}

// Chat runs one completion. onContent, if non-nil, receives streamed content
// deltas.
func (c *Client) Chat(ctx context.Context, messages []Message, tools []openai.ChatCompletionToolParam, onContent func(string)) (*ChatResult, error) {
	params := openai.ChatCompletionNewParams{
		Model:       openai.ChatModel(c.model),
		Messages:    messages,
		Temperature: openai.Float(c.temperature),
	}
	if len(tools) > 0 {
		params.Tools = tools
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("auto")}
	}
	if c.stream {
		return c.chatStream(ctx, params, onContent)
	}
	return c.chatOnce(ctx, params, onContent)
}

func (c *Client) chatOnce(ctx context.Context, params openai.ChatCompletionNewParams, onContent func(string)) (*ChatResult, error) {
	resp, err := c.api.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return c.buildResult("", nil), nil
	}
	m := resp.Choices[0].Message
	var calls []ToolCall
	for _, tc := range m.ToolCalls {
		calls = append(calls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	if onContent != nil && m.Content != "" {
		onContent(m.Content)
	}
	return c.buildResult(m.Content, calls), nil
}

func (c *Client) chatStream(ctx context.Context, params openai.ChatCompletionNewParams, onContent func(string)) (*ChatResult, error) {
	stream := c.api.Chat.Completions.NewStreaming(ctx, params)
	acc := map[int64]*ToolCall{}
	var order []int64
	var sb strings.Builder

	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			sb.WriteString(d.Content)
			if onContent != nil {
				onContent(d.Content)
			}
		}
		for _, tcd := range d.ToolCalls {
			a := acc[tcd.Index]
			if a == nil {
				a = &ToolCall{}
				acc[tcd.Index] = a
				order = append(order, tcd.Index)
			}
			if tcd.ID != "" {
				a.ID = tcd.ID
			}
			if tcd.Function.Name != "" {
				a.Name = tcd.Function.Name
			}
			a.Arguments += tcd.Function.Arguments
		}
	}
	if err := stream.Err(); err != nil {
		_ = stream.Close()
		return nil, err
	}
	_ = stream.Close()

	var calls []ToolCall
	for _, idx := range order {
		calls = append(calls, *acc[idx])
	}
	return c.buildResult(sb.String(), calls), nil
}

func (c *Client) buildResult(content string, calls []ToolCall) *ChatResult {
	return &ChatResult{
		Content:   content,
		ToolCalls: calls,
		Raw:       AssistantMessage(content, calls),
	}
}

// BuildTools generates the OpenAI tool schema from the command table.
func BuildTools() []openai.ChatCompletionToolParam {
	out := make([]openai.ChatCompletionToolParam, 0, len(command.Specs))
	for _, s := range command.Specs {
		props := map[string]any{}
		var required []string
		for _, p := range s.Params {
			props[p.Name] = map[string]any{"type": "string", "description": p.Description}
			if p.Required {
				required = append(required, p.Name)
			}
		}
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		out = append(out, openai.ChatCompletionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:        s.Name,
				Description: openai.String(s.Description),
				Parameters:  openai.FunctionParameters(schema),
			},
		})
	}
	return out
}
