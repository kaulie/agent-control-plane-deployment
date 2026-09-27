package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// panelVersion 是面板静态资源的指纹（index.html / app.js / styles.css 的内容哈希）。
//
// 为什么需要它：面板是**就地升级**的，文件名没有内容指纹，而已经打开的标签页里跑的
// 是升级前的 JS —— 轮询不会把页面上的 JS 换掉，于是升级后会出现「新控件在、填充它的
// 代码不在」的半新半旧页面（例如「部署机器」下拉渲染出来但一直是空的）。面板拿
// /api/meta 的 panelVersion 与自己加载时看到的值比：变了就自动刷新一次，跟上这次部署。
//
// 面板只有几十 KB，每次 /api/meta 现算一遍（本机控制面，可忽略）。
func (s *apiServer) panelVersion() string {
	if s == nil || s.cfg.WebDir == "" {
		return ""
	}
	h := sha1.New()
	found := false
	for _, name := range []string{"index.html", "app.js", "styles.css"} {
		b, err := os.ReadFile(filepath.Join(s.cfg.WebDir, name))
		if err != nil {
			continue
		}
		found = true
		_, _ = fmt.Fprintf(h, "%s:%d\n", name, len(b))
		_, _ = h.Write(b)
	}
	if !found {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
