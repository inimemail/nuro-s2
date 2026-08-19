package routes

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func isOpenAIMessagesGatewayPlatform(platform string) bool {
	return platform == service.PlatformOpenAI || platform == service.PlatformGrok
}

func isOpenAIChatCompletionsGatewayPlatform(platform string) bool {
	switch platform {
	case service.PlatformOpenAI, service.PlatformGrok, service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepSeek:
		return true
	default:
		return false
	}
}

// RegisterGatewayRoutes 注册 API 网关路由（Claude/OpenAI/Gemini 兼容）
func RegisterGatewayRoutes(
	r *gin.Engine,
	h *handler.Handlers,
	apiKeyAuth middleware.APIKeyAuthMiddleware,
	apiKeyService *service.APIKeyService,
	subscriptionService *service.SubscriptionService,
	opsService *service.OpsService,
	settingService *service.SettingService,
	cfg *config.Config,
) {
	bodyLimit := middleware.RequestBodyLimit(cfg.Gateway.MaxBodySize)
	clientRequestID := middleware.ClientRequestID()
	opsErrorLogger := handler.OpsErrorLoggerMiddleware(opsService)
	endpointNorm := handler.InboundEndpointMiddleware()

	// 未分组 Key 拦截中间件（按协议格式区分错误响应）
	requireGroupAnthropic := middleware.RequireGroupAssignment(settingService, middleware.AnthropicErrorWriter)
	requireGroupGoogle := middleware.RequireGroupAssignment(settingService, middleware.GoogleErrorWriter)
	compositeTarget := func(c *gin.Context) {
		apiKey, ok := middleware.GetAPIKeyFromContext(c)
		if ok && isCompositeGatewayAPIKey(apiKey) && c.Request != nil {
			if c.Request.Method == http.MethodGet &&
				strings.EqualFold(strings.TrimSpace(c.GetHeader("Upgrade")), "websocket") &&
				strings.Contains(c.Request.URL.Path, "responses") {
				c.Next()
				return
			}
			var body []byte
			var err error
			if c.Request.Body != nil {
				body, err = io.ReadAll(c.Request.Body)
			}
			if err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "invalid request body"}})
				return
			}
			model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
			if model == "" {
				model = strings.TrimSpace(c.Query("model"))
			}
			endpoint, fallbackModel, routed := compositeEndpointForPath(c.Request.URL.Path)
			if !routed {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
				c.Next()
				return
			}
			if model == "" {
				model = fallbackModel
			}
			resolver := service.DefaultCompositeRouteResolver()
			var decision service.CompositeRouteDecision
			if resolver != nil && apiKey.GroupID != nil {
				decision, err = resolver.Resolve(c.Request.Context(), *apiKey.GroupID, model, endpoint)
				if err != nil {
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{"type": "api_error", "message": "composite route lookup failed"}})
					return
				}
			}
			if !decision.Matched {
				if target, found := service.DetectCompositeModelPlatform(model); found {
					decision = service.CompositeRouteDecision{Matched: true, TargetPlatform: target, UpstreamModel: model}
				}
			}
			if !decision.Matched || decision.TargetPlatform == "" {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "Composite model route is not configured"}})
				return
			}
			ctx := service.WithResolvedTargetPlatform(c.Request.Context(), decision.TargetPlatform)
			if decision.UpstreamModel != "" {
				ctx = service.WithResolvedUpstreamModel(ctx, decision.UpstreamModel)
			}
			c.Request = c.Request.WithContext(ctx)
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}
		c.Next()
	}
	voiceHandler := func(endpoint string) gin.HandlerFunc {
		return func(c *gin.Context) { h.OpenAIGateway.GrokVoice(c, endpoint) }
	}
	customVoiceHandler := func(c *gin.Context) {
		endpoint := "custom-voices/" + c.Param("voice_id")
		if strings.HasSuffix(c.FullPath(), "/:voice_id/audio") {
			endpoint += "/audio"
		}
		h.OpenAIGateway.GrokVoice(c, endpoint)
	}

	isOpenAIResponsesCompatibleGatewayPlatform := func(c *gin.Context) bool {
		return isOpenAIMessagesGatewayPlatform(getGroupPlatform(c))
	}
	isOpenAIChatCompletionsGatewayPlatform := func(c *gin.Context) bool {
		return isOpenAIChatCompletionsGatewayPlatform(getGroupPlatform(c))
	}
	isOpenAIGatewayPlatform := func(c *gin.Context) bool {
		return getGroupPlatform(c) == service.PlatformOpenAI
	}
	countTokensHandler := func(c *gin.Context) {
		switch getGroupPlatform(c) {
		case service.PlatformOpenAI:
			h.OpenAIGateway.CountTokens(c)
		case service.PlatformGrok:
			h.OpenAIGateway.GrokCountTokens(c)
		default:
			h.Gateway.CountTokens(c)
		}
	}
	modelsHandler := func(c *gin.Context) {
		if isOpenAIGatewayPlatform(c) && c.Query("client_version") != "" {
			h.OpenAIGateway.CodexModels(c)
			return
		}
		h.Gateway.Models(c)
	}
	imagesHandler := func(c *gin.Context) {
		switch getGroupPlatform(c) {
		case service.PlatformOpenAI:
			h.OpenAIGateway.Images(c)
		case service.PlatformGrok:
			h.OpenAIGateway.GrokImages(c)
		default:
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Images API is not supported for this platform",
				},
			})
		}
	}
	openAIAsyncImageHandler := func(next gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			if getGroupPlatform(c) != service.PlatformOpenAI {
				service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
				c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"type": "not_found_error", "message": "Asynchronous image tasks are not supported for this platform"}})
				return
			}
			next(c)
		}
	}
	grokVideoGenerationHandler := func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformGrok {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Video API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.GrokVideoGeneration(c)
	}
	grokVideoStatusHandler := func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformGrok {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Video API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.GrokVideoStatus(c)
	}
	grokVideoContentHandler := func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformGrok {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Video API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.GrokVideoContent(c)
	}
	grokVideoEditHandler := func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformGrok {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Video API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.GrokVideoEdit(c)
	}
	grokVideoExtensionHandler := func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformGrok {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Video API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.GrokVideoExtension(c)
	}

	// API网关（Claude API兼容）
	gateway := r.Group("/v1")
	gateway.Use(bodyLimit)
	gateway.Use(clientRequestID)
	gateway.Use(opsErrorLogger)
	gateway.Use(endpointNorm)
	gateway.Use(gin.HandlerFunc(apiKeyAuth))
	guardResponsesSubpath := func(next gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			if !service.IsForwardableOpenAIResponsesRequestPath(c) {
				service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"type": "not_found_error", "message": "Unsupported responses subpath"}})
				return
			}
			next(c)
		}
	}
	gateway.GET("/sub2api/billing", h.Gateway.KeyBillingInfo)
	gateway.Use(compositeTarget, requireGroupAnthropic)
	{
		// /v1/messages: auto-route based on group platform
		gateway.POST("/messages", func(c *gin.Context) {
			if isOpenAIResponsesCompatibleGatewayPlatform(c) {
				h.OpenAIGateway.Messages(c)
				return
			}
			h.Gateway.Messages(c)
		})
		// OpenAI bridges upstream, Grok estimates locally without scheduling,
		// and Anthropic-compatible platforms retain their existing path.
		gateway.POST("/messages/count_tokens", countTokensHandler)
		gateway.GET("/models", modelsHandler)
		gateway.GET("/usage", h.Gateway.Usage)
		gateway.POST("/live", h.OpenAIGateway.Live)
		gateway.GET("/live/:call_id", h.OpenAIGateway.LiveSideband)
		// OpenAI Responses API: auto-route based on group platform
		gateway.POST("/responses", func(c *gin.Context) {
			if isOpenAIResponsesCompatibleGatewayPlatform(c) {
				h.OpenAIGateway.Responses(c)
				return
			}
			h.Gateway.Responses(c)
		})
		gateway.POST("/responses/*subpath", guardResponsesSubpath(func(c *gin.Context) {
			if isOpenAIResponsesCompatibleGatewayPlatform(c) {
				h.OpenAIGateway.Responses(c)
				return
			}
			h.Gateway.Responses(c)
		}))
		gateway.POST("/alpha/search", h.OpenAIGateway.AlphaSearch)
		gateway.GET("/responses", h.OpenAIGateway.ResponsesWebSocket)
		// OpenAI Chat Completions API: auto-route based on group platform
		gateway.POST("/chat/completions", func(c *gin.Context) {
			if isOpenAIChatCompletionsGatewayPlatform(c) {
				h.OpenAIGateway.ChatCompletions(c)
				return
			}
			h.Gateway.ChatCompletions(c)
		})
		gateway.POST("/embeddings", func(c *gin.Context) {
			if getGroupPlatform(c) != service.PlatformOpenAI {
				service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
				c.JSON(http.StatusNotFound, gin.H{
					"error": gin.H{
						"type":    "not_found_error",
						"message": "Embeddings API is not supported for this platform",
					},
				})
				return
			}
			h.OpenAIGateway.Embeddings(c)
		})
		gateway.POST("/images/generations", imagesHandler)
		gateway.POST("/images/edits", imagesHandler)
		gateway.POST("/images/generations/async", openAIAsyncImageHandler(h.OpenAIGateway.CreateAsyncImageGenerationTask))
		gateway.POST("/images/edits/async", openAIAsyncImageHandler(h.OpenAIGateway.CreateAsyncImageEditTask))
		gateway.GET("/images/tasks/:task_id", h.OpenAIGateway.GetAsyncImageTask)
		gateway.POST("/images/batches", h.BatchImage.Submit)
		gateway.GET("/images/batches", h.BatchImage.List)
		gateway.GET("/images/batches/models", h.BatchImage.Models)
		gateway.GET("/images/batches/:id", h.BatchImage.Get)
		gateway.GET("/images/batches/:id/items", h.BatchImage.Items)
		gateway.GET("/images/batches/:id/items/:custom_id/content", h.BatchImage.ItemContent)
		gateway.GET("/images/batches/:id/download", h.BatchImage.Download)
		gateway.POST("/images/batches/:id/cancel", h.BatchImage.Cancel)
		gateway.DELETE("/images/batches/:id", h.BatchImage.DeleteRecord)
		gateway.DELETE("/images/batches/:id/outputs", h.BatchImage.DeleteOutputs)
		gateway.POST("/videos/generations", grokVideoGenerationHandler)
		gateway.POST("/videos/edits", grokVideoEditHandler)
		gateway.POST("/videos/extensions", grokVideoExtensionHandler)
		gateway.GET("/videos/:request_id", grokVideoStatusHandler)
		gateway.GET("/videos/:request_id/content", grokVideoContentHandler)
		gateway.POST("/tts", voiceHandler("tts"))
		gateway.POST("/stt", voiceHandler("stt"))
		gateway.POST("/custom-voices", voiceHandler("custom-voices"))
		gateway.GET("/custom-voices", voiceHandler("custom-voices"))
		gateway.GET("/custom-voices/:voice_id", customVoiceHandler)
		gateway.GET("/custom-voices/:voice_id/audio", customVoiceHandler)
		gateway.PATCH("/custom-voices/:voice_id", customVoiceHandler)
		gateway.DELETE("/custom-voices/:voice_id", customVoiceHandler)
		gateway.GET("/realtime", h.OpenAIGateway.GrokRealtime)
		gateway.POST("/web_search", h.OpenAIGateway.GrokWebSearch)
		gateway.POST("/x_search", h.OpenAIGateway.GrokXSearch)
		gateway.GET("/image-tasks", func(c *gin.Context) {
			if getGroupPlatform(c) != service.PlatformOpenAI {
				service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
				c.JSON(http.StatusNotFound, gin.H{
					"error": gin.H{
						"type":    "not_found_error",
						"message": "Images API is not supported for this platform",
					},
				})
				return
			}
			h.OpenAIGateway.ListImageTasks(c)
		})
		gateway.POST("/image-tasks/generations", func(c *gin.Context) {
			if getGroupPlatform(c) != service.PlatformOpenAI {
				service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
				c.JSON(http.StatusNotFound, gin.H{
					"error": gin.H{
						"type":    "not_found_error",
						"message": "Images API is not supported for this platform",
					},
				})
				return
			}
			h.OpenAIGateway.CreateImageGenerationTask(c)
		})
		gateway.POST("/image-tasks/edits", func(c *gin.Context) {
			if getGroupPlatform(c) != service.PlatformOpenAI {
				service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
				c.JSON(http.StatusNotFound, gin.H{
					"error": gin.H{
						"type":    "not_found_error",
						"message": "Images API is not supported for this platform",
					},
				})
				return
			}
			h.OpenAIGateway.CreateImageEditTask(c)
		})
	}

	// Gemini 原生 API 兼容层（Gemini SDK/CLI 直连）
	gemini := r.Group("/v1beta")
	gemini.Use(bodyLimit)
	gemini.Use(clientRequestID)
	gemini.Use(opsErrorLogger)
	gemini.Use(endpointNorm)
	gemini.Use(middleware.APIKeyAuthWithSubscriptionGoogle(apiKeyService, subscriptionService, cfg))
	gemini.Use(requireGroupGoogle)
	{
		gemini.GET("/models", h.Gateway.GeminiV1BetaListModels)
		gemini.GET("/models/:model", h.Gateway.GeminiV1BetaGetModel)
		// Gin treats ":" as a param marker, but Gemini uses "{model}:{action}" in the same segment.
		gemini.POST("/models/*modelAction", h.Gateway.GeminiV1BetaModels)
	}

	// OpenAI Responses API（不带v1前缀的别名）— auto-route based on group platform
	responsesHandler := func(c *gin.Context) {
		if isOpenAIResponsesCompatibleGatewayPlatform(c) {
			h.OpenAIGateway.Responses(c)
			return
		}
		h.Gateway.Responses(c)
	}
	r.POST("/responses", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, responsesHandler)
	r.POST("/responses/*subpath", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, guardResponsesSubpath(responsesHandler))
	r.POST("/alpha/search", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), requireGroupAnthropic, h.OpenAIGateway.AlphaSearch)
	r.GET("/responses", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, h.OpenAIGateway.ResponsesWebSocket)
	r.GET("/models", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), requireGroupAnthropic, modelsHandler)
	codexDirect := r.Group("/backend-api/codex")
	codexDirect.Use(bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic)
	{
		codexDirect.POST("/realtime/calls", h.OpenAIGateway.Live)
		codexDirect.GET("/:call_id", h.OpenAIGateway.LiveSideband)
		codexDirect.POST("/responses", responsesHandler)
		codexDirect.POST("/responses/*subpath", guardResponsesSubpath(responsesHandler))
		codexDirect.POST("/alpha/search", h.OpenAIGateway.AlphaSearch)
		codexDirect.GET("/responses", h.OpenAIGateway.ResponsesWebSocket)
		codexDirect.GET("/models", h.OpenAIGateway.CodexModels)
	}
	// OpenAI Chat Completions API（不带v1前缀的别名）— auto-route based on group platform
	r.POST("/chat/completions", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, func(c *gin.Context) {
		if isOpenAIChatCompletionsGatewayPlatform(c) {
			h.OpenAIGateway.ChatCompletions(c)
			return
		}
		h.Gateway.ChatCompletions(c)
	})
	r.POST("/embeddings", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformOpenAI {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Embeddings API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.Embeddings(c)
	})
	r.POST("/images/generations", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, imagesHandler)
	r.POST("/images/edits", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, imagesHandler)
	r.POST("/images/generations/async", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, openAIAsyncImageHandler(h.OpenAIGateway.CreateAsyncImageGenerationTask))
	r.POST("/images/edits/async", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, openAIAsyncImageHandler(h.OpenAIGateway.CreateAsyncImageEditTask))
	r.GET("/images/tasks/:task_id", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), requireGroupAnthropic, h.OpenAIGateway.GetAsyncImageTask)
	r.POST("/videos/generations", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, grokVideoGenerationHandler)
	r.POST("/videos/edits", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, grokVideoEditHandler)
	r.POST("/videos/extensions", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, grokVideoExtensionHandler)
	r.GET("/videos/:request_id", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, grokVideoStatusHandler)
	r.GET("/videos/:request_id/content", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, grokVideoContentHandler)
	r.POST("/tts", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, voiceHandler("tts"))
	r.POST("/stt", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, voiceHandler("stt"))
	r.POST("/custom-voices", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, voiceHandler("custom-voices"))
	r.GET("/custom-voices", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, voiceHandler("custom-voices"))
	r.GET("/custom-voices/:voice_id", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, customVoiceHandler)
	r.GET("/custom-voices/:voice_id/audio", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, customVoiceHandler)
	r.PATCH("/custom-voices/:voice_id", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, customVoiceHandler)
	r.DELETE("/custom-voices/:voice_id", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, customVoiceHandler)
	r.GET("/realtime", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, h.OpenAIGateway.GrokRealtime)
	r.POST("/web_search", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, h.OpenAIGateway.GrokWebSearch)
	r.POST("/x_search", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, h.OpenAIGateway.GrokXSearch)
	r.GET("/image-tasks", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), requireGroupAnthropic, func(c *gin.Context) {
		platform := getGroupPlatform(c)
		if platform != service.PlatformOpenAI && platform != service.PlatformComposite {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Images API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.ListImageTasks(c)
	})
	r.POST("/image-tasks/generations", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformOpenAI {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Images API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.CreateImageGenerationTask(c)
	})
	r.POST("/image-tasks/edits", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformOpenAI {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Images API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.CreateImageEditTask(c)
	})

	// Antigravity 模型列表
	r.GET("/antigravity/models", gin.HandlerFunc(apiKeyAuth), requireGroupAnthropic, h.Gateway.AntigravityModels)

	// Antigravity 专用路由（仅使用 antigravity 账户，不混合调度）
	antigravityV1 := r.Group("/antigravity/v1")
	antigravityV1.Use(bodyLimit)
	antigravityV1.Use(clientRequestID)
	antigravityV1.Use(opsErrorLogger)
	antigravityV1.Use(endpointNorm)
	antigravityV1.Use(middleware.ForcePlatform(service.PlatformAntigravity))
	antigravityV1.Use(gin.HandlerFunc(apiKeyAuth))
	antigravityV1.Use(requireGroupAnthropic)
	{
		antigravityV1.POST("/messages", h.Gateway.Messages)
		antigravityV1.POST("/messages/count_tokens", h.Gateway.CountTokens)
		antigravityV1.GET("/models", h.Gateway.AntigravityModels)
		antigravityV1.GET("/usage", h.Gateway.Usage)
	}

	antigravityV1Beta := r.Group("/antigravity/v1beta")
	antigravityV1Beta.Use(bodyLimit)
	antigravityV1Beta.Use(clientRequestID)
	antigravityV1Beta.Use(opsErrorLogger)
	antigravityV1Beta.Use(endpointNorm)
	antigravityV1Beta.Use(middleware.ForcePlatform(service.PlatformAntigravity))
	antigravityV1Beta.Use(middleware.APIKeyAuthWithSubscriptionGoogle(apiKeyService, subscriptionService, cfg))
	antigravityV1Beta.Use(requireGroupGoogle)
	{
		antigravityV1Beta.GET("/models", h.Gateway.GeminiV1BetaListModels)
		antigravityV1Beta.GET("/models/:model", h.Gateway.GeminiV1BetaGetModel)
		antigravityV1Beta.POST("/models/*modelAction", h.Gateway.GeminiV1BetaModels)
	}

}

func isCompositeGatewayAPIKey(apiKey *service.APIKey) bool {
	return apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == service.PlatformComposite
}

// getGroupPlatform extracts the group platform from the API Key stored in context.
func getGroupPlatform(c *gin.Context) string {
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey.Group == nil {
		return ""
	}
	if apiKey.Group.Platform == service.PlatformComposite {
		if platform, resolved := service.ResolvedTargetPlatformFromContext(c.Request.Context()); resolved {
			return platform
		}
	}
	return apiKey.Group.Platform
}

func compositeEndpointForPath(path string) (endpoint, fallbackModel string, routed bool) {
	path = strings.TrimPrefix(strings.TrimSpace(path), "/v1")
	switch {
	case path == "/image-tasks" || strings.HasPrefix(path, "/images/tasks/") || strings.HasPrefix(path, "/images/batches"):
		// These are owner-scoped local task/batch operations. Creation endpoints
		// below still resolve a concrete provider from their request model.
		return "", "", false
	case strings.HasPrefix(path, "/messages/count_tokens"):
		return service.CompositeRouteEndpointCountTokens, "", true
	case strings.HasPrefix(path, "/messages"):
		return service.CompositeRouteEndpointMessages, "", true
	case strings.Contains(path, "/responses"):
		return service.CompositeRouteEndpointResponses, "", true
	case strings.Contains(path, "/chat/completions"):
		return service.CompositeRouteEndpointChatCompletions, "", true
	case strings.HasPrefix(path, "/embeddings"):
		return service.CompositeRouteEndpointEmbeddings, "", true
	case strings.HasPrefix(path, "/images") || strings.HasPrefix(path, "/image-tasks"):
		return service.CompositeRouteEndpointImages, "", true
	case strings.HasPrefix(path, "/videos"):
		return service.CompositeRouteEndpointVideos, "grok-imagine-video", true
	case strings.HasPrefix(path, "/tts") || strings.HasPrefix(path, "/stt") || strings.HasPrefix(path, "/custom-voices") || strings.HasPrefix(path, "/realtime"):
		return service.CompositeRouteEndpointVoice, "grok-voice-latest", true
	case strings.HasPrefix(path, "/web_search"):
		return service.CompositeRouteEndpointSearch, "grok-4.5", true
	case strings.HasPrefix(path, "/x_search"):
		return service.CompositeRouteEndpointSearch, "grok-4.5", true
	case strings.Contains(path, "/models/"):
		return service.CompositeRouteEndpointGemini, "", true
	default:
		return "", "", false
	}
}
