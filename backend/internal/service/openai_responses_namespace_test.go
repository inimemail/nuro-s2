package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestShouldStripOpenAIResponsesInputNamespaces(t *testing.T) {
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	tests := []struct {
		name        string
		account     *Account
		transport   OpenAIUpstreamTransport
		passthrough bool
		want        bool
	}{
		{name: "oauth http", account: oauth, transport: OpenAIUpstreamTransportHTTPSSE, want: true},
		{name: "api key http", account: apiKey, transport: OpenAIUpstreamTransportHTTPSSE, want: true},
		{name: "native oauth ws", account: oauth, transport: OpenAIUpstreamTransportResponsesWebsocketV2, want: false},
		{name: "native api key ws", account: apiKey, transport: OpenAIUpstreamTransportResponsesWebsocketV2, want: false},
		{name: "passthrough http", account: oauth, transport: OpenAIUpstreamTransportHTTPSSE, passthrough: true, want: false},
		{name: "passthrough ws", account: oauth, transport: OpenAIUpstreamTransportResponsesWebsocketV2, passthrough: true, want: false},
		{name: "other platform", account: &Account{Platform: PlatformGrok, Type: AccountTypeOAuth}, transport: OpenAIUpstreamTransportHTTPSSE, want: false},
		{name: "nil", transport: OpenAIUpstreamTransportHTTPSSE, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, shouldStripOpenAIResponsesInputNamespaces(tt.account, tt.transport, tt.passthrough))
		})
	}
}

func TestStripOpenAIResponsesInputNamespaces(t *testing.T) {
	body := []byte(`{"meta":9007199254740993,"tools":[{"namespace":"keep"}],"input":[{"type":"message","namespace":"remove","content":{"namespace":"nested"},"large":9007199254740993},{"type":"custom_tool_call","namespace":"remove"}]}`)
	got, err := stripOpenAIResponsesInputNamespaces(body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(got, "input.0.namespace").Exists())
	require.False(t, gjson.GetBytes(got, "input.1.namespace").Exists())
	require.Equal(t, "nested", gjson.GetBytes(got, "input.0.content.namespace").String())
	require.Equal(t, "keep", gjson.GetBytes(got, "tools.0.namespace").String())
	require.Equal(t, gjson.GetBytes(body, "meta").Raw, gjson.GetBytes(got, "meta").Raw)
	require.Equal(t, gjson.GetBytes(body, "input.0.large").Raw, gjson.GetBytes(got, "input.0.large").Raw)
}

func TestStripOpenAIResponsesInputNamespacesNoop(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"input":"text","namespace":"top"}`),
		[]byte(`{"input":{"namespace":"single"}}`),
		[]byte(`{"input":[{"content":{"namespace":"nested"}}]}`),
	} {
		got, err := stripOpenAIResponsesInputNamespaces(body)
		require.NoError(t, err)
		require.Equal(t, body, got)
	}
}
