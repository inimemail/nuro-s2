package typesafe

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validSystemOnePayload() []byte {
	return []byte(`{"model":"jev-latest","state":"hi","questions":{"q":{"type":"noul","instructions":"answer"}}}`)
}

func TestValidateSystemOneRequest(t *testing.T) {
	model, err := ValidateSystemOneRequest(validSystemOnePayload())
	require.NoError(t, err)
	require.Equal(t, JevLatestModel, model)

	_, err = ValidateSystemOneRequest([]byte(`{"model":"jev-latest","state":"hi","questions":{"q":{"type":"noul"}},"stream":true}`))
	require.ErrorIs(t, err, ErrStreamingUnsupported)

	_, err = ValidateSystemOneRequest([]byte(`{"model":"jev-latest","state":"hi","questions":{"q":{"type":"noul"}},"Model":"jev-latest"}`))
	require.Error(t, err)

	_, err = ValidateSystemOneRequest([]byte(`{"model":"other","state":"hi","questions":{"q":{"type":"noul"}}}`))
	require.Error(t, err)
	_, err = ValidateSystemOneRequest([]byte(`{"model":"jev-latest","state":"hi","questions":{"q":{"type":"choice","criteria":{"yes":"ok","yes":"duplicate"}}}}`))
	require.Error(t, err)
}

func TestDecodeSystemOneResponseEnforcesLimit(t *testing.T) {
	decoded, err := DecodeSystemOneResponse(strings.NewReader(`{"model":"jev-latest","usage":{"input_tokens":1,"output_tokens":"2"}}`))
	require.NoError(t, err)
	require.Equal(t, 1, decoded.Usage.InputTokens)
	require.Equal(t, 2, decoded.Usage.OutputTokens)
	_, err = DecodeSystemOneResponse(strings.NewReader(`{}`))
	require.Error(t, err)
	_, err = DecodeSystemOneResponse(strings.NewReader(`{"model":"jev-latest","usage":{}}`))
	require.Error(t, err)
	decoded, err = DecodeSystemOneResponse(strings.NewReader(`{"model":"jev-latest","usage":{"input_tokens":0,"output_tokens":0}}`))
	require.NoError(t, err)
	require.Zero(t, decoded.Usage.InputTokens)
	for _, body := range []string{
		`{"model":"jev-latest","usage":{"input_tokens":1.5,"output_tokens":2}}`,
		`{"model":"jev-latest","usage":{"input_tokens":-1,"output_tokens":2}}`,
		`{"model":"jev-latest","usage":{"input_tokens":1099511627777,"output_tokens":2}}`,
	} {
		_, err = DecodeSystemOneResponse(strings.NewReader(body))
		require.Error(t, err)
	}

	tooLarge := append([]byte(`{"model":"jev-latest","padding":"`), bytes.Repeat([]byte{'x'}, MaxSystemOneResponseBytes)...)
	tooLarge = append(tooLarge, []byte(`"}`)...)
	_, err = DecodeSystemOneResponse(bytes.NewReader(tooLarge))
	require.ErrorIs(t, err, ErrSystemOneResponseTooLarge)

	_, err = DecodeSystemOneResponse(strings.NewReader(`null`))
	require.Error(t, err)
	_, err = DecodeSystemOneResponse(strings.NewReader(`[]`))
	require.Error(t, err)
}
