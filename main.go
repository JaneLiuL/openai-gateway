package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

var (
	client = &http.Client{} // 全局HTTP客户端，复用连接
)

// 初始化配置
func initConfig() {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	if err := viper.ReadInConfig(); err != nil {
		panic(fmt.Errorf("读取配置文件失败: %s", err))
	}
}

// 实时获取JWT Token（无缓存，每次调用都请求）
func getJWTToken() (string, error) {
	// 1. 构建Token请求
	tokenURL := viper.GetString("token.url")
	tokenMethod := viper.GetString("token.method")
	tokenTimeout := viper.GetDuration("token.timeout")

	// 若Token服务需要请求体，可在这里添加（比如传客户端ID等）
	// tokenReqBody := bytes.NewBuffer([]byte(`{"client_id":"xxx","client_secret":"yyy"}`))
	// req, err := http.NewRequest(tokenMethod, tokenURL, tokenReqBody)
	req, err := http.NewRequest(tokenMethod, tokenURL, nil) // 无请求体的情况
	if err != nil {
		return "", fmt.Errorf("构建Token请求失败: %s", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 2. 发送Token请求
	client.Timeout = tokenTimeout
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求Token失败: %s", err)
	}
	defer resp.Body.Close()

	// 3. 解析Token响应（适配常见格式：token/access_token/jwt）
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

// 兼容OpenAI /chat/completions接口的代理处理函数
func openaiProxyHandler(c *gin.Context) {
	// 1. 实时获取JWT Token（无缓存）
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

	// 2. 读取AI模型的OpenAI格式请求体
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

	// 3. 自动补充默认参数（user/max_token）
	// 优先保留请求体中已有的值，无则用默认值
	defaultUser := viper.GetString("default_payload.user")
	defaultMaxToken := viper.GetInt("default_payload.max_token")

	if _, ok := openaiRequest["user"]; !ok {
		openaiRequest["user"] = defaultUser
	}
	if _, ok := openaiRequest["max_token"]; !ok {
		openaiRequest["max_tokens"] = defaultMaxToken // 对齐OpenAI的max_tokens字段名
	}

	// 4. 序列化拼接后的请求体
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

	// 5. 构建转发到目标服务的请求
	targetURL := viper.GetString("target.url")
	targetMethod := viper.GetString("target.method")

	req, err := http.NewRequest(targetMethod, targetURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("构建目标请求失败: %s", err),
				"type":    "internal_error",
			},
		})
		return
	}

	// 6. 添加JWT Token到请求头
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Content-Type", "application/json")

	// 7. 转发请求到目标服务
	targetTimeout := viper.GetDuration("server.timeout")
	client.Timeout = targetTimeout
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("转发请求到目标服务失败: %s", err),
				"type":    "downstream_error",
			},
		})
		return
	}
	defer resp.Body.Close()

	// 8. 读取目标服务响应
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("读取目标服务响应失败: %s", err),
				"type":    "internal_error",
			},
		})
		return
	}

	// 9. 透传目标服务的状态码和响应体（保持OpenAI格式）
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
}

func main() {
	// 初始化配置
	initConfig()

	// 初始化Gin引擎（生产环境可改为gin.ReleaseMode）
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// 注册兼容OpenAI的接口：/chat/completions
	r.POST("/chat/completions", openaiProxyHandler)

	// 启动代理服务
	port := viper.GetString("server.port")
	fmt.Printf("兼容OpenAI的代理服务器启动成功，监听端口：%s\n", port)
	fmt.Printf("调用地址：http://localhost:%s/chat/completions\n", port)
	if err := r.Run(":" + port); err != nil {
		panic(fmt.Errorf("启动代理服务失败: %s", err))
	}
}
