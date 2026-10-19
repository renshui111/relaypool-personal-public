package api

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/relaypool/relaypool/internal/armor"
)

// 只请求报告内的固定仓库地址，正文作为数据读取；不执行远端命令。
func (s *Server) researchFiles(ctx context.Context, id string) ([]armor.ResearchFile, error) {
	source, ok := armor.FindResearchSource(id)
	if !ok {
		return nil, fmt.Errorf("研究来源不存在")
	}
	if files := armor.BundledResearchFiles(id); len(files) > 0 {
		return files, nil
	}
	if source.Kind != "prompts" {
		return nil, fmt.Errorf("该项目是工具、手册或论文，请通过项目入口使用")
	}
	s.researchMu.Lock()
	cached := s.researchCache[id]
	s.researchMu.Unlock()
	if len(cached) > 0 {
		return append([]armor.ResearchFile{}, cached...), nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy, err := url.Parse(s.cfg.Upstream.ProxyURL); err == nil && s.cfg.Upstream.ProxyURL != "" {
		transport.Proxy = http.ProxyURL(proxy)
	}
	client := &http.Client{Timeout: 45 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, "GET", "https://codeload.github.com/"+source.Repository+"/zip/HEAD", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("无法下载研究来源，请检查网络或配置出站代理")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("来源下载失败（HTTP %d），可从本地导入文本", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("读取来源失败")
	}
	if len(raw) > 32<<20 {
		return nil, fmt.Errorf("来源包超过 32 MB，请从本地导入所需提示文件")
	}
	files, err := researchArchive(raw, id)
	if err != nil {
		return nil, err
	}
	s.researchMu.Lock()
	if s.researchCache == nil {
		s.researchCache = map[string][]armor.ResearchFile{}
	}
	s.researchCache[id] = files
	s.researchMu.Unlock()
	return files, nil
}

func researchArchive(raw []byte, id string) ([]armor.ResearchFile, error) {
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("来源不是有效的 ZIP 包")
	}
	if len(z.File) > 10000 {
		return nil, fmt.Errorf("来源包文件数量超过限制")
	}
	var files []armor.ResearchFile
	var license string
	var total int
	for _, entry := range z.File {
		name := entry.Name
		if _, rest, ok := strings.Cut(name, "/"); ok {
			name = rest
		}
		base := path.Base(name)
		ext := strings.ToLower(path.Ext(name))
		isLicense := strings.HasPrefix(strings.ToUpper(base), "LICENSE")
		if !isLicense && ext != ".md" && ext != ".txt" && ext != ".mkd" {
			continue
		}
		if !isLicense && (strings.HasPrefix(strings.ToUpper(base), "README") || strings.Contains(name, "..") || strings.HasPrefix(name, "/")) {
			continue
		}
		if entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > 1<<20 {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("来源文件损坏")
		}
		body, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
		reader.Close()
		if err != nil {
			return nil, fmt.Errorf("来源文件校验失败")
		}
		if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
			continue
		}
		if isLicense {
			if license == "" {
				license = string(body)
			}
			continue
		}
		total += len(body)
		if total > 8<<20 || len(files) >= 256 {
			return nil, fmt.Errorf("提示文本总量超过限制，请从本地选择文件导入")
		}
		files = append(files, armor.ResearchFile{SourceID: id, Path: name, Body: string(body), SHA256: armor.TextSHA256(string(body)), ArchiveSHA256: armor.TextSHA256(string(raw))})
	}
	for i := range files {
		files[i].LicenseText = license
		files[i].License = "未标注；请保留原项目说明"
		if license != "" {
			files[i].License = "见来源原始许可文本"
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("来源中未找到可导入的文本文件")
	}
	return files, nil
}

func (s *Server) handleResearchSources(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0)
	for _, source := range armor.ResearchSources() {
		items = append(items, map[string]any{"id": source.ID, "name": source.Name, "repository": source.Repository, "url": "https://github.com/" + source.Repository, "kind": source.Kind, "description": source.Description, "families": source.Families, "bundled_files": len(armor.BundledResearchFiles(source.ID))})
	}
	writeJSON(w, 200, map[string]any{"data": items})
}

func (s *Server) handleResearchFiles(w http.ResponseWriter, r *http.Request) {
	files, err := s.researchFiles(r.Context(), r.PathValue("source"))
	if err != nil {
		writeError(w, 400, "source_unavailable", err.Error())
		return
	}
	items := make([]map[string]any, 0, len(files))
	for _, file := range files {
		items = append(items, map[string]any{"path": file.Path, "family": file.Family, "sha256": file.SHA256, "bytes": len(file.Body), "license": file.License})
	}
	writeJSON(w, 200, map[string]any{"data": items})
}

func (s *Server) handleResearchPreview(w http.ResponseWriter, r *http.Request) {
	files, err := s.researchFiles(r.Context(), r.PathValue("source"))
	if err != nil {
		writeError(w, 400, "source_unavailable", err.Error())
		return
	}
	for _, file := range files {
		if file.Path == r.URL.Query().Get("path") {
			writeJSON(w, 200, file)
			return
		}
	}
	writeError(w, 404, "not_found", "来源文件不存在")
}
