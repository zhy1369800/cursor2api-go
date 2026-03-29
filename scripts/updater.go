package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

// 这个脚本用于从 Cursor 官网扫描最新的 SCRIPT_URL
// 运行方式: go run scripts/updater.go

func main() {
	urls := []string{
		"https://cursor.com/",
		"https://cursor.com/login",
		"https://cursor.com/settings",
	}

	fmt.Println("正在从多个页面扫描最新的 SCRIPT_URL...")
	fmt.Println("提示: 脚本会自动使用系统环境变量中的代理 (如 HTTP_PROXY)。")
	
	uniqueUrls := make(map[string]bool)
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}

	for _, target := range urls {
		fmt.Printf("扫描页面: %s ...\n", target)
		req, _ := http.NewRequest("GET", target, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// 匹配 /assets/index-*.js 或 /assets/chat-*.js
		re := regexp.MustCompile(`/assets/(?:chat|index)-[a-f0-9]+\.js`)
		matches := re.FindAllString(string(body), -1)

		for _, m := range matches {
			uniqueUrls["https://cursor.com"+m] = true
		}
	}

	if len(uniqueUrls) == 0 {
		fmt.Println("\n未发现匹配的脚本链接。请尝试在浏览器登录后手动查看 Network 开屏请求。")
		fmt.Println("推荐手动抓包地址: https://www.cursor.com/assets/index-75051939.js")
		return
	}

	fmt.Println("\n----------------找到的候选 URL----------------")
	for u := range uniqueUrls {
		fmt.Printf("URL: %s\n", u)
	}
	fmt.Println("----------------------------------------------")
	fmt.Println("\n请选择其中一个填入 .env 文件的 SCRIPT_URL 字段中。")
}
