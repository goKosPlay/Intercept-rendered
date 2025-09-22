package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/joho/godotenv"
	"github.com/yosssi/gohtml"
)

var (
	// 抓 data:[mime];base64,<data>
	// 允許大小寫與 +.- 等 (ex: image/svg+xml, image/vnd.microsoft.icon)
	reDataURI = regexp.MustCompile(`data:([a-zA-Z0-9.+\-\/]+);base64,([A-Za-z0-9+/=]+)`)
)

func main() {
	// 从 .env 文件加载环境变量
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	// 从环境变量获取目标 URL
	targetURL := os.Getenv("TARGET_URL")
	if targetURL == "" {
		log.Fatal("TARGET_URL is not set in .env file")
	}

	// 创建 chromedp 上下文
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	// 设置超时（前后端分离可能需要更长时间）
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// 存储渲染后的 HTML
	var html string
	// 存储外部 CSS 文件的 URL 和原始路径
	var cssURLs []string
	// 存储内联和动态 CSS
	var inlineStyles []string
	// 存储 <link> 标签的 href（用于提取原始路径）
	var linkHrefs []string

	// 监听网络请求以捕获外部 CSS 文件
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if e, ok := ev.(*network.EventResponseReceived); ok {
			if e.Type == network.ResourceTypeStylesheet {
				cssURLs = append(cssURLs, e.Response.URL)
			}
		}
	})

	// 定义 chromedp 任务
	err = chromedp.Run(ctx,
		// 启用网络请求拦截
		network.Enable(),
		// 导航到目标 URL
		chromedp.Navigate(targetURL),
		// 等待页面加载完成
		chromedp.ActionFunc(func(ctx context.Context) error {
			lctx, cancel := context.WithCancel(ctx)
			defer cancel()
			chromedp.ListenTarget(lctx, func(ev interface{}) {
				if _, ok := ev.(*page.EventLoadEventFired); ok {
					cancel()
				}
			})
			return nil
		}),
		// 等待 Vue3 根节点（可选，确保初始渲染）
		chromedp.WaitVisible(`#app`, chromedp.ByQuery),
		// 等待 .category-tab-item 渲染完成
		chromedp.WaitVisible(`.category-tab-item`, chromedp.ByQuery),
		// 确保 .category-tab-item 元素存在
		chromedp.ActionFunc(func(ctx context.Context) error {
			var nodes int
			err := chromedp.Evaluate(`document.querySelectorAll('.category-tab-item').length`, &nodes).Do(ctx)
			if err != nil {
				return err
			}
			if nodes == 0 {
				return fmt.Errorf("no .category-tab-item(elements found")
			}
			fmt.Printf("Found %d .category-tab-item elements\n", nodes)
			return nil
		}),
		// 获取渲染后的完整 HTML
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
		// 提取内联和动态 CSS（<style> 标签）
		chromedp.Evaluate(`Array.from(document.querySelectorAll('style')).map(s => s.textContent)`, &inlineStyles),
		// 提取 <link> 标签的 href
		chromedp.Evaluate(`Array.from(document.querySelectorAll('link[rel="stylesheet"]')).map(link => link.href)`, &linkHrefs),
	)
	if err != nil {
		log.Fatalf("Failed to run chromedp tasks: %v", err)
	}

	// 创建输出目录
	outputDir := "output"
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	// 保存 HTML 到文件
	//htmlFile := filepath.Join(outputDir, "rendered_page.html")
	//if err := os.WriteFile(htmlFile, []byte(html), 0644); err != nil {
	//	log.Fatalf("Failed to save HTML: %v", err)
	//}
	//fmt.Printf("Rendered HTML saved to %s\n", htmlFile)
	grabRenderedHTML(ctx, targetURL, "output/rendered_after_js.html")

	// 保存内联 CSS 到文件
	for i, style := range inlineStyles {
		filename := filepath.Join(outputDir, "inline", fmt.Sprintf("inline_style_%d.css", i+1))
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			log.Printf("Failed to create inline CSS directory: %v", err)
			continue
		}
		if err := os.WriteFile(filename, []byte(style), 0644); err != nil {
			log.Printf("Failed to save inline CSS %d: %v", i+1, err)
			continue
		}
		fmt.Printf("Saved inline CSS to %s\n", filename)
	}

	// 下载外部 CSS 文件，保留原始目录结构和文件名
	for i, cssURL := range cssURLs {
		// 从 linkHrefs 或 cssURL 提取原始路径
		originalPath := cssURL
		if i < len(linkHrefs) {
			originalPath = linkHrefs[i] // 优先使用 <link> 的 href
		}

		// 解析 URL 以提取路径
		u, err := url.Parse(originalPath)
		if err != nil {
			log.Printf("Failed to parse CSS URL %s: %v", cssURL, err)
			continue
		}

		// 提取路径和文件名
		path := u.Path
		if path == "" || path == "/" {
			path = fmt.Sprintf("external_css_%d.css", i+1)
		}

		// 构建输出路径，保留目录结构
		filename := filepath.Join(outputDir, "css", strings.TrimLeft(path, "/"))
		if err := downloadFile(cssURL, filename); err != nil {
			log.Printf("Failed to download external CSS %s: %v", cssURL, err)
			continue
		}
		fmt.Printf("Downloaded external CSS to %s\n", filename)
	}

	// 提取动态注入的 CSS（通过 style 属性）
	var dynamicStyles []string
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`
			Array.from(document.querySelectorAll('[style]'))
				.map(el => el.getAttribute('style'))
				.filter(style => style)
		`, &dynamicStyles),
	)
	if err != nil {
		log.Printf("Failed to extract dynamic styles: %v", err)
	} else {
		for i, style := range dynamicStyles {
			filename := filepath.Join(outputDir, "dynamic", fmt.Sprintf("dynamic_style_%d.css", i+1))
			if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
				log.Printf("Failed to create dynamic CSS directory: %v", err)
				continue
			}
			if err := os.WriteFile(filename, []byte(style), 0644); err != nil {
				log.Printf("Failed to save dynamic CSS %d:B %v", i+1, err)
				continue
			}
			fmt.Printf("Saved dynamic CSS to %s\n", filename)
		}
	}
}

func waitForNetworkIdle(idleFor time.Duration) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var inflight int
		lastActivity := time.Now()

		listen := func(ev interface{}) {
			switch ev.(type) {
			case *network.EventRequestWillBeSent:
				inflight++
				lastActivity = time.Now()
			case *network.EventLoadingFinished, *network.EventLoadingFailed:
				if inflight > 0 {
					inflight--
				}
				lastActivity = time.Now()
			}
		}

		chromedp.ListenTarget(ctx, listen)
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()

		timeout := time.After(30 * time.Second) // 给个总超时兜底
		for {
			select {
			case <-t.C:
				if inflight == 0 && time.Since(lastActivity) >= idleFor {
					return nil
				}
			case <-timeout:
				return fmt.Errorf("waitForNetworkIdle timeout")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
}

func grabRenderedHTML(ctx context.Context, url string, outFile string) error {
	var html string
	return chromedp.Run(ctx,
		network.Enable(),
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),   // DOM 就绪
		waitForNetworkIdle(1*time.Second),              // 网络空闲 1s（可按页面特性调大）
		chromedp.WaitVisible(`#app`, chromedp.ByQuery), // 你的 Vue 根节点
		// 如果你还要等某块动态内容出现，比如 .category-tab-item：
		// chromedp.WaitVisible(`.category-tab-item`, chromedp.ByQuery),

		// 拿 doctype + outerHTML（确保是“JS 后”的 DOM）
		chromedp.Evaluate(`
(() => {
  const dt = document.doctype;
  const doctype = dt ? "<!DOCTYPE " + dt.name
    + (dt.publicId ? ' PUBLIC "' + dt.publicId + '"' : '')
    + (!dt.publicId && dt.systemId ? ' SYSTEM' : '')
    + (dt.systemId ? ' "' + dt.systemId + '"' : '')
    + ">\n" : "";
  return doctype + document.documentElement.outerHTML;
})()
`, &html),
		chromedp.ActionFunc(func(ctx context.Context) error {
			pretty := gohtml.Format(html)
			assetDir := filepath.Join("output", "assets")
			// 先把 base64 data: 圖片抽出→落地→替換引用
			if err := os.MkdirAll(assetDir, 0755); err != nil {
				log.Fatalf("mkdir assets failed: %v", err)
			}
			processedHTML, saved, err := extractAndReplaceDataURIs(html, assetDir)
			if err != nil {
				log.Fatalf("extract/replace data URIs failed: %v", err)
			}
			fmt.Printf("🖼️  另存 base64 圖片 %d 張到 %s/\n", saved, assetDir)
			pretty = gohtml.Format(processedHTML)
			return os.WriteFile(outFile, []byte(pretty), 0644)
		}),
	)
}

// 返回：處理後 HTML、保存數量、錯誤
func extractAndReplaceDataURIs(html string, assetDir string) (string, int, error) {
	if html == "" {
		return html, 0, nil
	}
	// 去重：相同內容只存一次
	seen := map[string]string{} // hash -> relative path
	saved := 0

	processed := reDataURI.ReplaceAllStringFunc(html, func(match string) string {
		m := reDataURI.FindStringSubmatch(match)
		if len(m) != 3 {
			return match
		}
		mimeType := strings.ToLower(m[1])
		b64 := m[2]

		// 先對 base64 內容做 hash（去重 & 穩定檔名）
		h := sha1.Sum([]byte(b64))
		hashPrefix := fmt.Sprintf("%x", h)[:12]

		// 已存過就直接替換為既有路徑
		if rel, ok := seen[hashPrefix]; ok {
			return rel
		}

		// 解碼
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			// 解不出來就原樣返回
			return match
		}

		ext := guessExt(mimeType)
		filename := fmt.Sprintf("b64_%s%s", hashPrefix, ext)
		outPath := filepath.Join(assetDir, filename)
		if err := os.WriteFile(outPath, data, 0644); err != nil {
			// 寫檔失敗就原樣返回
			return match
		}
		rel := filepath.ToSlash(filepath.Join("assets", filename)) // 用於 HTML 內的相對路徑
		seen[hashPrefix] = rel
		saved++
		return rel
	})

	return processed, saved, nil
}

// 猜副檔名
func guessExt(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	case "image/avif":
		return ".avif"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return ".ico"
	default:
		// 盡量從 mime 驗證（需要有對應的副檔名註冊）
		if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
			return exts[0]
		}
		// 實在不認得就給個通用
		return ".bin"
	}
}

// downloadFile 下载文件到指定路径
func downloadFile(url, filename string) error {
	// 创建输出目录
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return err
	}

	// 发送 HTTP 请求
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	// 创建文件
	out, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer out.Close()

	// 写入文件
	_, err = io.Copy(out, resp.Body)
	return err
}
