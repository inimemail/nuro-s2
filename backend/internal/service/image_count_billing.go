package service

import (
	"crypto/sha256"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func imageCountBillingApplies(key *APIKey, endpoint bool) bool {
	return endpoint && key != nil && key.Group != nil && key.Group.ImageCountBilling && key.Group.AllowImageGeneration &&
		(key.Group.Platform == PlatformOpenAI || key.Group.Platform == PlatformComposite)
}

const geminiObservedImageCountKey = "gemini_observed_image_count"
const geminiImageObservationKey = "gemini_image_observation"
const geminiImageObservationLimit = 1024

type geminiImageObservation struct {
	seen     map[[32]byte]struct{}
	overflow bool
}

// Retain only bounded fingerprints, never provider images. Occurrences within
// one candidate distinguish repeated images in a snapshot; repeated snapshots
// and distinct incremental chunks share the same deduplication set.
func observeGeminiImageCount(c *gin.Context, body []byte) {
	if c == nil || len(body) == 0 {
		return
	}
	stored, _ := c.Get(geminiImageObservationKey)
	state, _ := stored.(*geminiImageObservation)
	if state != nil && state.overflow {
		return
	}
	var observe func(gjson.Result)
	observe = func(value gjson.Result) {
		if value.IsArray() {
			value.ForEach(func(_, item gjson.Result) bool { observe(item); return state == nil || !state.overflow })
			return
		}
		if nested := value.Get("response"); nested.IsObject() {
			value = nested
		}
		for ci, candidate := range value.Get("candidates").Array() {
			candidateID := strconv.Itoa(ci)
			if index := candidate.Get("index"); index.Exists() {
				candidateID = index.String()
			}
			var occurrences map[[32]byte]int
			for _, part := range candidate.Get("content.parts").Array() {
				for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
					data := part.Get(key)
					mime := data.Get("mimeType").String()
					if mime == "" {
						mime = data.Get("mime_type").String()
					}
					content := data.Get("data").String()
					if content == "" {
						content = data.Get("fileUri").String()
					}
					if content == "" {
						content = data.Get("file_uri").String()
					}
					if !strings.HasPrefix(strings.ToLower(mime), "image/") || content == "" {
						continue
					}
					if state == nil {
						state = &geminiImageObservation{seen: make(map[[32]byte]struct{})}
						c.Set(geminiImageObservationKey, state)
					}
					if occurrences == nil {
						occurrences = make(map[[32]byte]int)
					}
					digest := sha256.Sum256([]byte(candidateID + "\x00" + content))
					occurrences[digest]++
					identity := sha256.Sum256(append(digest[:], []byte(strconv.Itoa(occurrences[digest]))...))
					if _, exists := state.seen[identity]; !exists {
						if len(state.seen) >= geminiImageObservationLimit {
							state.overflow = true
							c.Set(geminiObservedImageCountKey, 0)
							return
						}
						state.seen[identity] = struct{}{}
					}
					break
				}
			}
		}
	}
	observe(gjson.ParseBytes(body))
	if state != nil && !state.overflow {
		c.Set(geminiObservedImageCountKey, len(state.seen))
	}
}
