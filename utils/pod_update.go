package utils

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

// Kubernetes API请求体结构（仅更新annotations）
type patchBody struct {
	Metadata metadata `json:"metadata"`
}

type metadata struct {
	Annotations map[string]string `json:"annotations"`
}

func UpdatePodMetricsPort(port int) error {
	// 1. 获取环境变量中的Pod信息（通常在Pod内运行时由K8s自动注入）
	fmt.Println("pod port:%v", port)
	k8sServeurl := os.Getenv("K8S_SERVER_URL")
	podName := os.Getenv("POD_NAME")
	namespace := os.Getenv("POD_NAMESPACE")
	if podName == "" || namespace == "" || k8sServeurl == "" {
		fmt.Println("请设置POD_NAME和POD_NAMESPACE环境变量")
		return fmt.Errorf("请设置POD_NAME和POD_NAMESPACE环境变量")
	}

	// 2. 读取Kubernetes API的访问令牌和CA证书（Pod内路径）
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		fmt.Printf("读取令牌失败: %v\n", err)
		return fmt.Errorf("读取令牌失败: %v", err)
	}

	caCert, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		fmt.Printf("读取CA证书失败: %v\n", err)
		return fmt.Errorf("读取CA证书失败: %v", err)
	}

	// 3. 构建请求URL
	// apiServer := "https://kubernetes.default.svc" // 集群内API Server地址
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", k8sServeurl, namespace, podName)

	// 4. 准备要更新的annotations
	annotations := map[string]string{
		"prometheus_io_port": strconv.Itoa(port),
	}

	// 5. 构建PATCH请求体（使用JSON Merge Patch格式）
	patchData := patchBody{
		Metadata: metadata{
			Annotations: annotations,
		},
	}
	jsonData, err := json.Marshal(patchData)
	if err != nil {
		fmt.Printf("JSON序列化失败: %v\n", err)
		return fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 6. 配置HTTP客户端（信任集群内CA证书）
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: caCertPool,
			},
		},
	}

	// 7. 创建PATCH请求
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("创建请求失败: %v\n", err)
		return fmt.Errorf("创建请求失败: %v", err)
	}

	// 8. 设置请求头（认证和内容类型）
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Content-Type", "application/merge-patch+json") // 必须使用此类型进行部分更新

	// 9. 发送请求并处理响应
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("发送请求失败: %v\n", err)
		return fmt.Errorf("发送请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 10. 读取响应内容
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("读取响应失败: %v\n", err)
		return fmt.Errorf("读取响应失败: %v", err)
	}

	// 11. 检查响应状态
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		fmt.Printf("更新失败，状态码: %d，响应: %s\n", resp.StatusCode, string(body))
		return fmt.Errorf("更新失败，状态码: %d，响应: %s", resp.StatusCode, string(body))
	}

	fmt.Printf("Pod annotations更新成功！响应: %s\n", string(body))
	return nil
}
