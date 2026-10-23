// Command traeprobe 直连 Trae Remote 通道，落盘原始 SSE 事件，用于排查正文/思考/用量字段。
//
// 用法:
//
//	traeprobe -model glm-5.3-flash -prompt "你好" -out raw.txt
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/traechat"
)

func main() {
	model := flag.String("model", "deepseek-v4.1-flash", "Trae 裸模型名")
	prompt := flag.String("prompt", "只回复两个字：你好", "提示词")
	out := flag.String("out", "trae_probe_raw.txt", "原始事件落盘路径")
	dataDir := flag.String("data-dir", "", "Trae 数据目录（空 = %APPDATA%\\TraeWorkAssistant）")
	timeout := flag.Duration("timeout", 150*time.Second, "整体超时")
	mode := flag.String("mode", "raw", "raw=直连抓原始事件；relay=走中继（同网关路径）")
	flag.Parse()

	client := traechat.NewClient(nil)
	pool := traechat.NewPool(client, *dataDir)
	cat := traechat.NewCatalogCache(pool, 30*time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *mode == "relay" {
		runRelay(ctx, pool, cat, *model, *prompt)
		return
	}

	models, err := cat.Models(ctx)
	if err != nil {
		fatalf("拉取模型目录失败: %v", err)
	}
	var mi traechat.ModelInfo
	found := false
	for _, m := range models {
		if m.Name == *model || m.DisplayName == *model {
			mi, found = m, true
			break
		}
	}
	if !found {
		fatalf("目录里没有模型 %q（共 %d 个）", *model, len(models))
	}
	fmt.Printf("model=%s credits=%s ctx=%d\n", mi.Name, mi.CreditsLabel(), mi.ContextWindow)

	acct, err := pool.Acquire(ctx)
	if err != nil {
		fatalf("取号失败: %v", err)
	}
	defer pool.Release(acct)
	fmt.Printf("account=%s user=%s\n", acct.Name, acct.UserID)

	query, _ := json.Marshal(*prompt)
	sid, mid, status, err := client.CreateSession(traechat.CreateRequest{
		JWT:       acct.JWT,
		UserID:    acct.UserID,
		DeviceID:  acct.DeviceID,
		Model:     mi,
		AgentType: traechat.AgentTypeAgent,
		Query: traechat.FlattenQuery([]traechat.Message{
			{Role: "user", Content: query},
		}),
		ChatMode: "code",
	})
	if err != nil {
		fatalf("建会话失败(status=%d): %v", status, err)
	}
	fmt.Printf("session=%s message=%s\n", sid, mid)
	defer func() { _ = client.Stop(acct.JWT, sid, mid) }()

	f, err := os.Create(*out)
	if err != nil {
		fatalf("创建输出文件失败: %v", err)
	}
	defer f.Close()

	seq := 0
	start := time.Now()
	err = client.StreamEvents(acct.JWT, sid, mid, func(ev traechat.EventTrae) error {
		seq++
		var pretty any
		if json.Unmarshal(ev.Data, &pretty) == nil {
			b, _ := json.MarshalIndent(pretty, "", "  ")
			fmt.Fprintf(f, "=== #%d %s ===\n%s\n\n", seq, ev.Name, b)
		} else {
			fmt.Fprintf(f, "=== #%d %s ===\n%s\n\n", seq, ev.Name, ev.Data)
		}
		return nil
	})
	fmt.Printf("events=%d elapsed=%s stream_err=%v\n", seq, time.Since(start).Round(time.Millisecond), err)
	if err != nil {
		fmt.Printf("NOTE: 流结束带错误: %v\n", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// runRelay 走与网关完全一致的中继路径，打印最终正文/思考/用量。
func runRelay(ctx context.Context, pool *traechat.Pool, cat *traechat.CatalogCache, model, prompt string) {
	relay := traechat.NewRelay(pool, cat)
	relay.Pool = pool
	content, _ := json.Marshal(prompt)
	start := time.Now()
	res, err := relay.Complete(ctx, traechat.ChatRequest{
		Model: model,
		Messages: []traechat.Message{
			{Role: "user", Content: content},
		},
	})
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("relay 失败(%s): %v\n", elapsed, err)
		os.Exit(2)
	}
	fmt.Printf("elapsed=%s\ncontent=%q\nreasoning_len=%d\nusage=%+v hasUsage=%v\n",
		elapsed, res.Content, len(res.Reasoning), res.Usage, res.HasUsage)
}
