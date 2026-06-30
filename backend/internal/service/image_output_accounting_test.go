package service

import "testing"

func TestOpenAIImageOutputCounter_TextOnlyMessage(t *testing.T) {
	sseBody := `data: {"type":"response.output_item.done","item":{"id":"item_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]}}

data: {"type":"response.completed","response":{"id":"resp_123","output":[{"id":"item_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":5}}}

data: [DONE]`

	if count := countOpenAIImageOutputsFromSSEBody(sseBody); count != 0 {
		t.Fatalf("expected 0 images for text-only message, got %d", count)
	}
}

func TestOpenAIImageOutputCounter_DataArrayFalsePositive(t *testing.T) {
	sseWithNonImageData := `data: {"type":"response.completed","response":{"id":"resp_1","output":[{"id":"item_1","type":"message","content":[{"type":"output_text","text":"Hello"}]}]},"data":[{"id":"not_an_image","status":"done"}]}

data: [DONE]`
	if count := countOpenAIImageOutputsFromSSEBody(sseWithNonImageData); count != 0 {
		t.Fatalf("expected 0 images for data array without image output, got %d", count)
	}

	sseWithImageData := `data: {"type":"response.completed","response":{"id":"resp_1","output":[]},"data":[{"url":"https://example.com/img.png"}]}

data: [DONE]`
	if count := countOpenAIImageOutputsFromSSEBody(sseWithImageData); count != 1 {
		t.Fatalf("expected 1 image for data array with image URL, got %d", count)
	}
}

func TestOpenAIImageOutputCounter_JSONResponseDataArrayFalsePositive(t *testing.T) {
	jsonWithNonImageData := `{
		"id": "resp_1",
		"object": "response",
		"status": "completed",
		"output": [
			{
				"id": "item_1",
				"type": "message",
				"role": "assistant",
				"status": "completed",
				"content": [{"type": "output_text", "text": "Hello"}]
			}
		],
		"data": [
			{"id": "not_an_image", "status": "done"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	if count := countOpenAIResponseImageOutputsFromJSONBytes([]byte(jsonWithNonImageData)); count != 0 {
		t.Fatalf("expected 0 images for data array without image output, got %d", count)
	}

	jsonWithImageData := `{
		"id": "resp_1",
		"object": "response",
		"output": [],
		"data": [
			{"url": "https://example.com/img.png"}
		]
	}`
	if count := countOpenAIResponseImageOutputsFromJSONBytes([]byte(jsonWithImageData)); count != 1 {
		t.Fatalf("expected 1 image for data array with image URL, got %d", count)
	}
}

func TestOpenAIImageOutputCounter_ImageGenerationCompletedWithoutResult(t *testing.T) {
	sseBody := `data: {"type":"image_generation.completed","item":{"type":"image_generation.completed","id":"call_1"}}

data: [DONE]`
	if count := countOpenAIImageOutputsFromSSEBody(sseBody); count != 0 {
		t.Fatalf("expected 0 images without image result, got %d", count)
	}
}
