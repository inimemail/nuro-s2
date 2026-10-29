package service

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiObservedImagesIncrementalAndSnapshots(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	event := func(images string) []byte {
		return []byte(`{"candidates":[{"index":0,"content":{"parts":[` + images + `]}}]}`)
	}
	image := func(data string) string {
		return fmt.Sprintf(`{"inlineData":{"mimeType":"image/png","data":%q}}`, data)
	}
	observeGeminiImageCount(c, event(image("one")))
	observeGeminiImageCount(c, event(image("two")))
	require.Equal(t, 2, c.GetInt(geminiObservedImageCountKey))
	observeGeminiImageCount(c, event(image("one")+","+image("two")))
	require.Equal(t, 2, c.GetInt(geminiObservedImageCountKey))
	observeGeminiImageCount(c, event(image("one")+","+image("one")))
	require.Equal(t, 3, c.GetInt(geminiObservedImageCountKey))
	resetGeminiResponseSignal(c)
	require.Zero(t, c.GetInt(geminiObservedImageCountKey))
	observeGeminiImageCount(c, []byte(`[{"response":{"candidates":[{"index":1,"content":{"parts":[{"file_data":{"mime_type":"image/jpeg","file_uri":"image-file"}}]}}]}}]`))
	require.Equal(t, 1, c.GetInt(geminiObservedImageCountKey))
	for i := 0; i < geminiImageObservationLimit; i++ {
		observeGeminiImageCount(c, event(image(fmt.Sprint(i))))
	}
	require.Zero(t, c.GetInt(geminiObservedImageCountKey), "overflow must remain unknown")
}

func TestImageCountBillingOnlyDedicatedOptIn(t *testing.T) {
	key := &APIKey{Group: &Group{Platform: PlatformOpenAI, AllowImageGeneration: true, ImageCountBilling: true}}
	require.True(t, imageCountBillingApplies(key, true))
	require.False(t, imageCountBillingApplies(key, false))
	key.Group.Platform = PlatformAnthropic
	require.False(t, imageCountBillingApplies(key, true))
	key.Group.Platform = PlatformComposite
	require.True(t, imageCountBillingApplies(key, true))
	key.Group.AllowImageGeneration = false
	require.False(t, imageCountBillingApplies(key, true))
}

func TestGeminiTextResponseDoesNotAllocateImageObservation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	for _, body := range []string{
		`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`,
		`[{"response":{"candidates":[{"content":{"parts":[{"text":"world"}]}}]}}]`,
		`{"usageMetadata":{"promptTokenCount":12}}`,
	} {
		observeGeminiImageCount(c, []byte(body))
		_, present := c.Get(geminiImageObservationKey)
		require.False(t, present)
	}
	resetGeminiResponseSignal(c)
	_, present := c.Get(geminiImageObservationKey)
	require.False(t, present)
}
