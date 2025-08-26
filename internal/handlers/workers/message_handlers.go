package workers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/sirupsen/logrus"

	"github.com/prefeitura-rio/app-eai-agent-gateway/internal/config"
	"github.com/prefeitura-rio/app-eai-agent-gateway/internal/middleware"
	"github.com/prefeitura-rio/app-eai-agent-gateway/internal/models"
	"github.com/prefeitura-rio/app-eai-agent-gateway/internal/services"
)

// MessageHandlerDependencies contains dependencies needed for message processing
type MessageHandlerDependencies struct {
	Logger             *logrus.Logger
	Config             *config.Config
	RedisService       *services.RedisService
	GoogleAgentService *services.GoogleAgentEngineService
	TranscribeService  TranscribeServiceInterface
	MessageFormatter   MessageFormatterInterface
	OTelWorkerWrapper  *middleware.OTelWorkerWrapper // Optional OTel wrapper
}

// TranscribeServiceInterface defines audio transcription operations
type TranscribeServiceInterface interface {
	TranscribeAudio(ctx context.Context, audioURL string) (string, error)
	IsAudioURL(url string) bool
	ValidateAudioURL(url string) error
}

// TranscribeServiceAdapter adapts the services.TranscribeService to the handler interface
type TranscribeServiceAdapter struct {
	service *services.TranscribeService
}

// NewTranscribeServiceAdapter creates a new adapter
func NewTranscribeServiceAdapter(service *services.TranscribeService) *TranscribeServiceAdapter {
	return &TranscribeServiceAdapter{service: service}
}

// TranscribeAudio implements the interface by calling TranscribeFromURL
func (a *TranscribeServiceAdapter) TranscribeAudio(ctx context.Context, audioURL string) (string, error) {
	if a.service == nil {
		return "", fmt.Errorf("transcribe service is not available")
	}
	result, err := a.service.TranscribeFromURL(ctx, audioURL)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// IsAudioURL checks if the URL appears to be an audio file
func (a *TranscribeServiceAdapter) IsAudioURL(url string) bool {
	// Check for audio file extensions regardless of service availability
	// This allows detection even when transcribe service is not configured
	audioExtensions := []string{".mp3", ".wav", ".m4a", ".aac", ".ogg", ".flac", ".wma"}
	for _, ext := range audioExtensions {
		if strings.HasSuffix(strings.ToLower(url), ext) {
			return true
		}
	}
	return false
}

// ValidateAudioURL validates the audio URL format
func (a *TranscribeServiceAdapter) ValidateAudioURL(url string) error {
	if a.service == nil {
		return fmt.Errorf("transcribe service is not available")
	}
	if url == "" {
		return fmt.Errorf("audio URL cannot be empty")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("audio URL must start with http:// or https://")
	}
	return nil
}

// MessageFormatterInterface defines message formatting operations
type MessageFormatterInterface interface {
	FormatForWhatsApp(ctx context.Context, response *models.AgentResponse) (string, error)
	FormatErrorMessage(ctx context.Context, err error) string
	ValidateMessageContent(content string) error
}

// CreateUserMessageHandler creates a handler for user messages
func CreateUserMessageHandler(deps *MessageHandlerDependencies) func(context.Context, amqp.Delivery) error {
	return func(ctx context.Context, delivery amqp.Delivery) error {
		logger := deps.Logger.WithFields(logrus.Fields{
			"handler":      "user_message",
			"delivery_tag": delivery.DeliveryTag,
			"message_id":   delivery.MessageId,
		})

		logger.Info("Processing user message")

		// Parse the queue message
		var queueMsg models.QueueMessage
		if err := json.Unmarshal(delivery.Body, &queueMsg); err != nil {
			logger.WithError(err).Error("Failed to unmarshal queue message")
			// Return error for malformed messages (service layer will handle nack)
			return err
		}

		logger = logger.WithFields(logrus.Fields{
			"queue_message_id": queueMsg.ID,
			"user_number":      queueMsg.UserNumber,
			"message_type":     queueMsg.Type,
			"provider":         queueMsg.Provider,
		})

		// Update task status to processing
		if err := deps.RedisService.SetTaskStatus(ctx, queueMsg.ID, string(models.TaskStatusProcessing), deps.Config.Redis.TaskStatusTTL); err != nil {
			logger.WithError(err).Error("Failed to update task status to processing")
		}

		// Process the user message with optional OTel tracing
		var response string
		var err error

		if deps.OTelWorkerWrapper != nil {
			// Wrap with OpenTelemetry tracing
			err = deps.OTelWorkerWrapper.WrapWorkerTask(ctx, "user_message_worker", "process_user_message", func(tracedCtx context.Context) error {
				response, err = processUserMessage(tracedCtx, &queueMsg, deps)
				return err
			})
		} else {
			// Process without tracing
			response, err = processUserMessage(ctx, &queueMsg, deps)
		}

		if err != nil {
			logger.WithError(err).Error("Failed to process user message")

			// Store error in Redis
			errorKey := "task:error:" + queueMsg.ID
			if redisErr := deps.RedisService.Set(ctx, errorKey, err.Error(), deps.Config.Redis.TaskStatusTTL); redisErr != nil {
				logger.WithError(redisErr).Error("Failed to store error in Redis")
			}

			// Update task status to failed
			if statusErr := deps.RedisService.SetTaskStatus(ctx, queueMsg.ID, string(models.TaskStatusFailed), deps.Config.Redis.TaskStatusTTL); statusErr != nil {
				logger.WithError(statusErr).Error("Failed to update task status to failed")
			}

			// Return success to prevent infinite retries (service layer will handle ack)
			return nil
		}

		// Store the response in Redis
		if err := deps.RedisService.SetTaskResult(ctx, queueMsg.ID, response, deps.Config.Redis.TaskResultTTL); err != nil {
			logger.WithError(err).Error("Failed to store task result")
			// Don't fail the message processing for Redis storage issues
		}

		// Update task status to completed
		if err := deps.RedisService.SetTaskStatus(ctx, queueMsg.ID, string(models.TaskStatusCompleted), deps.Config.Redis.TaskStatusTTL); err != nil {
			logger.WithError(err).Error("Failed to update task status to completed")
		}

		logger.WithField("response_length", len(response)).Info("User message processed successfully")

		// Return success (service layer will handle acknowledgment)
		return nil
	}
}

// isAudioURL checks if the URL appears to be an audio file (standalone function)
func isAudioURL(url string) bool {
	audioExtensions := []string{".mp3", ".wav", ".m4a", ".aac", ".ogg", ".flac", ".wma"}
	for _, ext := range audioExtensions {
		if strings.HasSuffix(strings.ToLower(url), ext) {
			return true
		}
	}
	return false
}

// processUserMessage handles the actual user message processing logic (matches Python process_user_message)
func processUserMessage(ctx context.Context, msg *models.QueueMessage, deps *MessageHandlerDependencies) (string, error) {
	logger := deps.Logger.WithField("function", "processUserMessage")

	logger.WithFields(logrus.Fields{
		"user_number":          msg.UserNumber,
		"message_length":       len(msg.Message),
		"has_previous_message": msg.PreviousMessage != nil,
		"provider":             msg.Provider,
	}).Info("Processing user message")

	logger.Info("DEBUG: Starting processUserMessage function execution")

	// Validate provider - currently only support google_agent_engine
	if msg.Provider != "google_agent_engine" {
		logger.WithField("provider", msg.Provider).Error("Unsupported provider")
		return "", fmt.Errorf("unsupported provider: %s (currently only 'google_agent_engine' is supported)", msg.Provider)
	}

	// Check if Google Agent service is available
	if deps.GoogleAgentService == nil {
		logger.Error("Google Agent Engine service not available")
		return "", fmt.Errorf("google Agent Engine service is required but not available")
	}

	// Handle audio transcription if message is an audio URL
	message := msg.Message
	var transcriptText *string

	// Check if message is an audio URL (independent of service availability)
	isAudioURL := isAudioURL(message)

	if isAudioURL {
		logger.WithField("audio_url", message).Info("Detected audio URL, attempting transcription")

		if deps.TranscribeService == nil {
			logger.Warn("Transcribe service not available, using fallback")
			message = "Ajuda"
		} else {
			transcript, err := deps.TranscribeService.TranscribeAudio(ctx, message)
			if err != nil {
				logger.WithError(err).Warn("Failed to transcribe audio, using fallback")
				// Fallback to not block the flow (matches Python logic)
				message = "Ajuda"
			} else if transcript != "" && strings.TrimSpace(transcript) != "" && transcript != "Áudio sem conteúdo reconhecível" {
				transcriptText = &transcript
				message = transcript
				logger.WithField("transcript_length", len(transcript)).Info("Audio transcribed successfully")
			} else {
				logger.Warn("Transcription returned no useful content, using fallback")
				message = "Ajuda"
			}
		}
	}

	// Validate message content
	if deps.MessageFormatter != nil {
		if err := deps.MessageFormatter.ValidateMessageContent(message); err != nil {
			logger.WithError(err).Error("Message content validation failed")
			return "", fmt.Errorf("invalid message content: %w", err)
		}
	}

	// Get or create thread for user (thread ID corresponds to agent ID in Python logic)
	threadID, err := deps.GoogleAgentService.GetOrCreateThread(ctx, msg.UserNumber)
	if err != nil {
		logger.WithError(err).Error("Failed to get or create thread")
		return "", fmt.Errorf("failed to get thread: %w", err)
	}

	logger.WithField("thread_id", threadID).Info("Using thread for conversation")

	// Send message to Google Agent Engine
	// The Google Agent Engine automatically handles previous message context via thread ID
	agentResponse, err := deps.GoogleAgentService.SendMessage(ctx, threadID, message)
	if err != nil {
		logger.WithError(err).Error("Failed to send message to Google Agent Engine")
		return "", fmt.Errorf("failed to get AI response: %w", err)
	}

	// Parse Google's raw JSON response immediately after getting it from Google Agent Engine
	logger.WithField("raw_response_length", len(agentResponse.Content)).Debug("Processing Google Agent Engine response")

	// Clean the JSON string first to handle newlines and other whitespace issues
	cleanedResponse := strings.ReplaceAll(agentResponse.Content, "\n", "")
	cleanedResponse = strings.ReplaceAll(cleanedResponse, "\r", "")
	cleanedResponse = strings.TrimSpace(cleanedResponse)

	var parsedResponse map[string]interface{}
	if err := json.Unmarshal([]byte(cleanedResponse), &parsedResponse); err != nil {
		logger.WithFields(logrus.Fields{
			"error":       err.Error(),
			"raw_json":    cleanedResponse,
			"json_length": len(cleanedResponse),
		}).Error("Failed to parse Google Agent Engine JSON response")
		return "", fmt.Errorf("failed to parse AI response JSON: %w", err)
	}

	// Extract the 'output' field which contains the messages
	output, exists := parsedResponse["output"]
	if !exists {
		logger.Error("No 'output' field found in Google Agent Engine response")
		return "", fmt.Errorf("invalid Google Agent Engine response format - missing 'output' field")
	}

	outputMap, ok := output.(map[string]interface{})
	if !ok {
		logger.Error("'output' field is not a map in Google Agent Engine response")
		return "", fmt.Errorf("invalid Google Agent Engine response format - 'output' is not an object")
	}

	// Extract messages array from the output structure
	var transformedMessages []interface{}
	if messagesArray, exists := outputMap["messages"]; exists {
		transformedMessages = transformGoogleAgentMessages(deps.Logger, messagesArray)
	} else {
		// Fallback to empty messages if no messages field
		logger.Warn("No 'messages' field found in output, using empty array")
		transformedMessages = []interface{}{}
	}

	// Generate agent ID based on user number
	agentID := "user_" + msg.UserNumber

	// Set agent_id in the usage statistics message
	if len(transformedMessages) > 0 {
		if lastMsg, ok := transformedMessages[len(transformedMessages)-1].(map[string]interface{}); ok {
			if msgType, exists := lastMsg["message_type"]; exists && msgType == "usage_statistics" {
				lastMsg["agent_id"] = agentID
			}
		}
	}

	// Apply WhatsApp formatting to individual message content
	transformedMessages = applyWhatsAppFormattingToMessages(deps.Logger, deps.MessageFormatter, transformedMessages)

	// Build the final response data to match Python API structure
	processedData := models.ProcessedMessageData{
		Messages:    transformedMessages,
		AgentID:     agentID,
		ProcessedAt: msg.ID, // Use message ID as processed_at identifier
		Status:      "done",
	}

	// Convert the processed data to JSON for storage in Redis
	processedBytes, err := json.Marshal(processedData)
	if err != nil {
		logger.WithError(err).Error("Failed to marshal processed data to JSON")
		return "", fmt.Errorf("failed to marshal processed response: %w", err)
	}

	processedResponse := string(processedBytes)

	// Log successful processing (matches Python log format)
	logger.WithFields(logrus.Fields{
		"thread_id":           threadID,
		"agent_id":            agentID,
		"raw_response_length": len(agentResponse.Content),
		"processed_length":    len(processedResponse),
		"messages_count":      len(transformedMessages),
		"had_transcript":      transcriptText != nil,
	}).Info("Successfully processed user message with full transformation pipeline")

	return processedResponse, nil
}

// transformGoogleAgentMessages transforms Google Agent Engine messages to Python API format
func transformGoogleAgentMessages(logger *logrus.Logger, messagesData interface{}) []interface{} {
	var transformedMessages []interface{}

	// Handle both slice and single message cases
	var messagesList []interface{}

	switch v := messagesData.(type) {
	case []interface{}:
		messagesList = v
	case interface{}:
		messagesList = []interface{}{v}
	default:
		logger.Warn("Unexpected messages data type, returning empty array")
		return transformedMessages
	}

	for _, msgData := range messagesList {
		msgMap, ok := msgData.(map[string]interface{})
		if !ok {
			continue
		}

		// Transform Google Agent Engine message to Python API format
		transformedMsg := map[string]interface{}{
			"id":                      msgMap["id"],
			"date":                    nil,
			"session_id":              nil,
			"time_since_last_message": nil,
			"name":                    msgMap["name"],
			"otid":                    msgMap["id"], // Use same ID as otid
			"sender_id":               nil,
			"step_id":                 "step-" + generateStepID(), // Generate step ID
			"is_err":                  nil,
			"model_name":              extractModelName(msgMap),
			"finish_reason":           extractFinishReason(msgMap),
			"avg_logprobs":            extractAvgLogprobs(msgMap),
			"usage_metadata":          extractUsageMetadata(msgMap),
			"message_type":            mapMessageType(msgMap),
			"content":                 msgMap["content"],
		}

		// Add type-specific fields
		if msgType := mapMessageType(msgMap); msgType == "tool_call_message" {
			if toolCalls, exists := msgMap["tool_calls"].([]interface{}); exists && len(toolCalls) > 0 {
				if toolCall, ok := toolCalls[0].(map[string]interface{}); ok {
					transformedMsg["tool_call"] = map[string]interface{}{
						"name":         toolCall["name"],
						"arguments":    toolCall["args"],
						"tool_call_id": toolCall["id"],
					}
				}
			}
		} else if msgType == "tool_return_message" {
			// For tool messages, extract tool return information
			if name, exists := msgMap["name"]; exists {
				transformedMsg["tool_return"] = msgMap["content"]
				transformedMsg["status"] = "success"
				transformedMsg["tool_call_id"] = msgMap["tool_call_id"]
				transformedMsg["stdout"] = nil
				transformedMsg["stderr"] = nil
				transformedMsg["name"] = name
			}
		}

		transformedMessages = append(transformedMessages, transformedMsg)
	}

	// Add usage statistics message at the end (matching Python API)
	usageStats := map[string]interface{}{
		"message_type":      "usage_statistics",
		"completion_tokens": 0,
		"prompt_tokens":     0,
		"total_tokens":      0,
		"step_count":        len(transformedMessages),
		"steps_messages":    nil,
		"run_ids":           nil,
		"agent_id":          "", // Will be filled by calling function
		"processed_at":      time.Now().Format(time.RFC3339),
		"status":            "done",
		"model_names":       []string{},
	}
	transformedMessages = append(transformedMessages, usageStats)

	return transformedMessages
}

// Helper functions for message transformation
func extractModelName(msgMap map[string]interface{}) interface{} {
	if responseMetadata, exists := msgMap["response_metadata"].(map[string]interface{}); exists {
		if modelName, exists := responseMetadata["model_name"]; exists {
			return modelName
		}
	}
	return nil
}

func extractFinishReason(msgMap map[string]interface{}) interface{} {
	if responseMetadata, exists := msgMap["response_metadata"].(map[string]interface{}); exists {
		if finishReason, exists := responseMetadata["finish_reason"]; exists {
			return finishReason
		}
	}
	return nil
}

func extractAvgLogprobs(msgMap map[string]interface{}) interface{} {
	if responseMetadata, exists := msgMap["response_metadata"].(map[string]interface{}); exists {
		if avgLogprobs, exists := responseMetadata["avg_logprobs"]; exists {
			return avgLogprobs
		}
	}
	return nil
}

func extractUsageMetadata(msgMap map[string]interface{}) interface{} {
	if responseMetadata, exists := msgMap["response_metadata"].(map[string]interface{}); exists {
		if usageMetadata, exists := responseMetadata["usage_metadata"]; exists {
			// Transform to Python API format
			if usageMap, ok := usageMetadata.(map[string]interface{}); ok {
				result := map[string]interface{}{
					"prompt_token_count":     usageMap["input_tokens"],
					"candidates_token_count": usageMap["output_tokens"],
					"total_token_count":      usageMap["total_tokens"],
				}

				// Safely extract nested fields with proper type conversion
				if outputDetails, exists := usageMap["output_token_details"].(map[string]interface{}); exists {
					if reasoning, exists := outputDetails["reasoning"]; exists {
						if reasoningInt, ok := reasoning.(int); ok {
							result["thoughts_token_count"] = float64(reasoningInt)
						}
					}
				}

				if inputDetails, exists := usageMap["input_token_details"].(map[string]interface{}); exists {
					if cacheRead, exists := inputDetails["cache_read"]; exists {
						if cacheReadInt, ok := cacheRead.(int); ok {
							result["cached_content_token_count"] = float64(cacheReadInt)
						}
					}
				}

				// Convert other fields to float64 to match Python API
				if inputTokens, exists := usageMap["input_tokens"]; exists {
					if inputInt, ok := inputTokens.(int); ok {
						result["prompt_token_count"] = float64(inputInt)
					}
				}
				if outputTokens, exists := usageMap["output_tokens"]; exists {
					if outputInt, ok := outputTokens.(int); ok {
						result["candidates_token_count"] = float64(outputInt)
					}
				}
				if totalTokens, exists := usageMap["total_tokens"]; exists {
					if totalInt, ok := totalTokens.(int); ok {
						result["total_token_count"] = float64(totalInt)
					}
				}

				return result
			}
		}
	}
	return nil
}

func mapMessageType(msgMap map[string]interface{}) string {
	msgType, exists := msgMap["type"].(string)
	if !exists {
		return "user_message" // Default fallback
	}

	switch msgType {
	case "human":
		return "user_message"
	case "ai":
		// Check if it has tool calls
		if toolCalls, exists := msgMap["tool_calls"].([]interface{}); exists && len(toolCalls) > 0 {
			return "tool_call_message"
		}
		return "assistant_message"
	case "tool":
		return "tool_return_message"
	default:
		return "user_message"
	}
}

// generateStepID generates a random step ID in the format expected by Python API
func generateStepID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // Ignore error as rand.Read from crypto/rand always returns len(b), nil
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// applyWhatsAppFormattingToMessages applies WhatsApp formatting to individual message content
func applyWhatsAppFormattingToMessages(logger *logrus.Logger, messageFormatter MessageFormatterInterface, messages []interface{}) []interface{} {
	if messageFormatter == nil {
		logger.Warn("MessageFormatter is nil, skipping WhatsApp formatting")
		return messages
	}

	for i, msgInterface := range messages {
		if msgMap, ok := msgInterface.(map[string]interface{}); ok {
			// Only format message content, not metadata
			if content, exists := msgMap["content"].(string); exists && content != "" {
				// Create a temporary AgentResponse to use with the FormatForWhatsApp service
				tempResponse := &models.AgentResponse{
					Content:   content,
					MessageID: "temp", // Not used by the formatter
					ThreadID:  "temp", // Not used by the formatter
				}

				// Apply WhatsApp formatting using the proper service
				formattedContent, err := messageFormatter.FormatForWhatsApp(context.Background(), tempResponse)
				if err != nil {
					logger.WithError(err).Warn("Failed to format message content for WhatsApp, using original content")
					formattedContent = content // Fallback to original content
				}

				msgMap["content"] = formattedContent
				messages[i] = msgMap
			}
		}
	}
	return messages
}
