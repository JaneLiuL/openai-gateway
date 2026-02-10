package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var (
	client = &http.Client{}
	// 固定Header名
	correlationIDHeader = "x-correlation-id"
	userSessionIDHeader = "x-usersession-id"

	// 全局配置（从环境变量读取）
	config = struct {
		// Token服务配置
		TokenURL              string
		TokenMethod           string
		TokenTimeout          time.Duration
		TokenPayloadTokenType string // 新增：token_type的值
		// 目标服务配置
		TargetURL    string
		TargetMethod string
		// 默认请求体参数
		DefaultUser     string
		DefaultMaxToken int
		// 代理服务配置
		ServerPort    string
		ServerTimeout time.Duration
	}{}
)

// 定义Delta结构体（带JSON tag）
type Delta struct {
	Content string `json:"content,omitempty"`
	Role    string `json:"role,omitempty"`
}

// OpenAI标准流式响应结构
type OpenAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Delta        Delta  `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
}

// OpenAI标准非流式响应结构
type OpenAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
		Index        int    `json:"index"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

// 获取环境变量，若不存在则返回默认值
func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

// 初始化配置（从环境变量读取）
func initConfig() {
	// 1. Token服务配置
	config.TokenURL = getEnv("TOKEN_URL", "http://localhost:8000/api/get-jwt")
	config.TokenMethod = getEnv("TOKEN_METHOD", "POST")
	// 新增：token_type的值（默认SESSION_TOKEN）
	config.TokenPayloadTokenType = getEnv("TOKEN_PAYLOAD_TOKEN_TYPE", "SESSION_TOKEN")
	// 解析超时时间（默认5秒）
	tokenTimeoutStr := getEnv("TOKEN_TIMEOUT", "5s")
	timeout, err := time.ParseDuration(tokenTimeoutStr)
	if err != nil {
		fmt.Printf("TOKEN_TIMEOUT格式错误，使用默认值5s: %s\n", err)
		config.TokenTimeout = 5 * time.Second
	} else {
		config.TokenTimeout = timeout
	}

	// 2. 目标服务配置
	config.TargetURL = getEnv("TARGET_URL", "http://localhost:8001/api/ai-call")
	config.TargetMethod = getEnv("TARGET_METHOD", "POST")

	// 3. 默认请求体参数
	config.DefaultUser = getEnv("DEFAULT_USER", "ai_model_user")
	maxTokenStr := getEnv("DEFAULT_MAX_TOKEN", "2000")
	maxToken, err := strconv.Atoi(maxTokenStr)
	if err != nil {
		fmt.Printf("DEFAULT_MAX_TOKEN格式错误，使用默认值2000: %s\n", err)
		config.DefaultMaxToken = 2000
	} else {
		config.DefaultMaxToken = maxToken
	}

	// 4. 代理服务配置
	config.ServerPort = getEnv("SERVER_PORT", "8080")
	serverTimeoutStr := getEnv("SERVER_TIMEOUT", "10s")
	serverTimeout, err := time.ParseDuration(serverTimeoutStr)
	if err != nil {
		fmt.Printf("SERVER_TIMEOUT格式错误，使用默认值10s: %s\n", err)
		config.ServerTimeout = 10 * time.Second
	} else {
		config.ServerTimeout = serverTimeout
	}

	// 打印配置（调试用，生产环境可注释）
	fmt.Println("=== 代理服务配置 ===")
	fmt.Printf("TokenURL: %s\n", config.TokenURL)
	fmt.Printf("TokenPayloadTokenType: %s\n", config.TokenPayloadTokenType)
	fmt.Printf("TargetURL: %s\n", config.TargetURL)
	fmt.Printf("ServerPort: %s\n", config.ServerPort)
	fmt.Println("====================")
}

// 生成随机字符串（UUID v4）
func generateRandomString() string {
	return uuid.New().String()
}

// 实时获取JWT Token（新增JSON payload）
func getJWTToken() (string, error) {
	// 构建Token请求的JSON payload
	tokenPayload := map[string]string{
		"token_type": config.TokenPayloadTokenType, // 核心：添加token_type字段
	}
	payloadBytes, err := json.Marshal(tokenPayload)
	if err != nil {
		return "", fmt.Errorf("序列化Token请求体失败: %s", err)
	}

	// 构建Token请求（带payload）
	req, err := http.NewRequest(config.TokenMethod, config.TokenURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return "", fmt.Errorf("构建Token请求失败: %s", err)
	}
	req.Header.Set("Content-Type", "application/json") // 确保Content-Type正确

	// 发送Token请求
	client.Timeout = config.TokenTimeout
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求Token失败: %s", err)
	}
	defer resp.Body.Close()

	// 解析Token响应
	var tokenResp map[string]interface{}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取Token响应失败: %s", err)
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("解析Token响应失败（响应体：%s）: %s", string(body), err)
	}

	// 兼容多字段名
	var token string
	if t, ok := tokenResp["token"]; ok {
		token = t.(string)
	} else if t, ok := tokenResp["access_token"]; ok {
		token = t.(string)
	} else if t, ok := tokenResp["jwt"]; ok {
		token = t.(string)
	} else {
		return "", fmt.Errorf("Token响应无有效字段（响应体：%s）", string(body))
	}

	if token == "" {
		return "", fmt.Errorf("获取到空的JWT Token")
	}
	return token, nil
}

// 将目标服务响应转换为OpenAI格式（非流式）
func convertToOpenAIResponse(targetResp []byte, model string) ([]byte, error) {
	// 解析目标服务响应
	var targetData map[string]interface{}
	if err := json.Unmarshal(targetResp, &targetData); err != nil {
		return nil, fmt.Errorf("解析目标响应失败: %s", err)
	}

	// 构建OpenAI响应
	openAIResp := OpenAIResponse{
		ID:      fmt.Sprintf("chatcmpl-%s", strings.ReplaceAll(generateRandomString(), "-", "")),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
			Index        int    `json:"index"`
		}{
			{
				Message: struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				}{
					Role:    "assistant",
					Content: fmt.Sprintf("%v", targetData["content"]),
				},
				FinishReason: fmt.Sprintf("%v", targetData["finish_reason"]),
				Index:        0,
			},
		},
	}

	// 可选：添加Usage字段
	if promptTokens, ok := targetData["prompt_tokens"]; ok {
		openAIResp.Usage.PromptTokens, _ = strconv.Atoi(fmt.Sprintf("%v", promptTokens))
	}
	if completionTokens, ok := targetData["completion_tokens"]; ok {
		openAIResp.Usage.CompletionTokens, _ = strconv.Atoi(fmt.Sprintf("%v", completionTokens))
	}
	openAIResp.Usage.TotalTokens = openAIResp.Usage.PromptTokens + openAIResp.Usage.CompletionTokens

	// 序列化为JSON
	openAIRespBytes, err := json.Marshal(openAIResp)
	if err != nil {
		return nil, fmt.Errorf("序列化OpenAI响应失败: %s", err)
	}
	return openAIRespBytes, nil
}

// 处理流式响应转换（目标SSE→OpenAI SSE）
func handleStreamResponse(c *gin.Context, resp *http.Response, model string) error {
	// 设置OpenAI流式响应Header
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	// 逐行读取目标服务的流式响应
	reader := bufio.NewReader(resp.Body)
	chunkID := fmt.Sprintf("chatcmpl-%s", strings.ReplaceAll(generateRandomString(), "-", ""))
	created := time.Now().Unix()

	for {
		// 读取一行
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 发送结束chunk
				finishChunk := OpenAIStreamChunk{
					ID:      chunkID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []struct {
						Delta        Delta  `json:"delta"`
						FinishReason string `json:"finish_reason,omitempty"`
					}{
						{
							Delta:        Delta{},
							FinishReason: "stop",
						},
					},
				}
				finishBytes, _ := json.Marshal(finishChunk)
				c.Writer.WriteString(fmt.Sprintf("data: %s\n\n", string(finishBytes)))
				c.Writer.Flush()
				return nil
			}
			return fmt.Errorf("读取流式响应失败: %s", err)
		}

		// 解析目标服务的SSE行
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		dataStr := strings.TrimPrefix(line, "data: ")
		if dataStr == "[DONE]" {
			continue
		}

		// 解析目标chunk
		var targetChunk map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &targetChunk); err != nil {
			continue
		}

		// 转换为OpenAI chunk格式
		openAIChunk := OpenAIStreamChunk{
			ID:      chunkID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []struct {
				Delta        Delta  `json:"delta"`
				FinishReason string `json:"finish_reason,omitempty"`
			}{
				{
					Delta: Delta{
						Content: fmt.Sprintf("%v", targetChunk["content"]),
						Role:    "assistant",
					},
					FinishReason: "",
				},
			},
		}

		// 发送到客户端
		chunkBytes, err := json.Marshal(openAIChunk)
		if err != nil {
			continue
		}
		c.Writer.WriteString(fmt.Sprintf("data: %s\n\n", string(chunkBytes)))
		c.Writer.Flush()

		// 检查客户端是否断开连接
		if c.Request.Context().Err() != nil {
			return nil
		}
	}
}

// 核心代理处理函数
func openaiProxyHandler(c *gin.Context) {
	// 1. 获取JWT Token
	token, err := getJWTToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("获取Token失败: %s", err),
				"type":    "token_error",
			},
		})
		return
	}

	// 2. 读取OpenAI格式请求
	var openaiRequest map[string]interface{}
	if err := c.ShouldBindJSON(&openaiRequest); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("解析请求体失败: %s", err),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	// 3. 补充默认参数
	if _, ok := openaiRequest["user"]; !ok {
		openaiRequest["user"] = config.DefaultUser
	}
	if _, ok := openaiRequest["max_token"]; !ok {
		openaiRequest["max_tokens"] = config.DefaultMaxToken
	}

	// 4. 获取模型名和流式标识
	model := "gpt-3.5-turbo"
	if m, ok := openaiRequest["model"]; ok {
		model = fmt.Sprintf("%v", m)
	}
	isStream := false
	if s, ok := openaiRequest["stream"]; ok {
		isStream, _ = strconv.ParseBool(fmt.Sprintf("%v", s))
	}

	// 5. 序列化请求体
	payloadBytes, err := json.Marshal(openaiRequest)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("序列化请求体失败: %s", err),
				"type":    "internal_error",
			},
		})
		return
	}

	// 6. 构建目标请求
	req, err := http.NewRequest(config.TargetMethod, config.TargetURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("构建目标请求失败: %s", err),
				"type":    "internal_error",
			},
		})
		return
	}

	// 7. 添加所有要求的Header
	req.Header.Set("X-Trust-Token", token)
	req.Header.Set(correlationIDHeader, generateRandomString())
	req.Header.Set(userSessionIDHeader, generateRandomString())
	req.Header.Set("Token_Type", "SESSION_TOKEN")
	req.Header.Set("Content-Type", "application/json")

	// 8. 转发请求
	client.Timeout = config.ServerTimeout
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("转发请求失败: %s", err),
				"type":    "downstream_error",
			},
		})
		return
	}
	defer resp.Body.Close()

	// 9. 处理响应（流式/非流式）
	if isStream {
		if err := handleStreamResponse(c, resp, model); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": fmt.Sprintf("处理流式响应失败: %s", err),
					"type":    "stream_error",
				},
			})
		}
	} else {
		// 非流式响应：转换为OpenAI格式
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": fmt.Sprintf("读取目标响应失败: %s", err),
					"type":    "internal_error",
				},
			})
			return
		}

		// 转换为OpenAI格式
		openAIResp, err := convertToOpenAIResponse(respBody, model)
		if err != nil {
			// 转换失败时透传原始响应
			c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
			return
		}

		// 返回OpenAI格式响应
		c.Data(resp.StatusCode, "application/json", openAIResp)
	}
}

// 健康检查
func healthCheckHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "healthy",
		"service": "openai-proxy",
		"time":    time.Now().Format(time.RFC3339),
	})
}

func main() {
	// 初始化配置（从环境变量）
	initConfig()

	// 初始化Gin引擎
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// 路由
	r.GET("/health", healthCheckHandler)
	r.POST("/chat/completions", openaiProxyHandler)

	// 启动服务
	fmt.Printf("OpenAI兼容代理服务启动成功 | 端口：%s\n", config.ServerPort)
	fmt.Printf("接口：POST http://0.0.0.0:%s/chat/completions\n", config.ServerPort)
	fmt.Printf("健康检查：GET http://0.0.0.0:%s/health\n", config.ServerPort)

	if err := r.Run(":" + config.ServerPort); err != nil {
		panic(fmt.Errorf("启动服务失败: %s", err))
	}
}
