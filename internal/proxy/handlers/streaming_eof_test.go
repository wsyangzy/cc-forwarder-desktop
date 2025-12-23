package handlers

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsStreamingEOFError 测试 EOF 错误检测逻辑
func TestIsStreamingEOFError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "流式传输 EOF 错误",
			err:      errors.New("stream_status:error:model:claude-sonnet-4-5-20250929: unexpected EOF"),
			expected: true,
		},
		{
			name:     "流式传输 EOF 错误（大写）",
			err:      errors.New("stream_status:error:model:test: EOF"),
			expected: true,
		},
		{
			name:     "连接阶段 EOF（无 stream_status 前缀）",
			err:      errors.New("Post \"https://api.example.com\": EOF"),
			expected: false,
		},
		{
			name:     "普通连接错误",
			err:      errors.New("connection refused"),
			expected: false,
		},
		{
			name:     "超时错误",
			err:      errors.New("context deadline exceeded"),
			expected: false,
		},
		{
			name:     "nil 错误",
			err:      nil,
			expected: false,
		},
		{
			name:     "其他流式错误（非 EOF）",
			err:      errors.New("stream_status:error:model:test: connection reset"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isStreamingEOFError(tt.err)
			if result != tt.expected {
				t.Errorf("isStreamingEOFError(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

// TestSendAnthropicRetryableError 测试错误响应格式
func TestSendAnthropicRetryableError(t *testing.T) {
	recorder := httptest.NewRecorder()

	sendAnthropicRetryableError(recorder, recorder, "Stream interrupted (EOF), please retry")

	body := recorder.Body.String()

	// 验证 SSE 格式
	if !strings.Contains(body, "event: error\n") {
		t.Error("响应应包含 'event: error'")
	}

	// 验证 JSON 格式
	if !strings.Contains(body, `"type":"error"`) {
		t.Error("响应应包含 type:error")
	}

	if !strings.Contains(body, `"type":"overloaded_error"`) {
		t.Error("响应应包含 overloaded_error 类型")
	}

	if !strings.Contains(body, "Stream interrupted") {
		t.Error("响应应包含错误消息")
	}

	t.Logf("生成的响应:\n%s", body)
}

// TestEscapeJSONString 测试 JSON 字符串转义
func TestEscapeJSONString(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "hello"},
		{`say "hi"`, `say \"hi\"`},
		{"line1\nline2", `line1\nline2`},
		{"path\\to\\file", `path\\to\\file`},
		{"tab\there", `tab\there`},
	}

	for _, tt := range tests {
		result := escapeJSONString(tt.input)
		if result != tt.expected {
			t.Errorf("escapeJSONString(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
