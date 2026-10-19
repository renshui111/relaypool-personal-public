// Command presetpack 只解析已下载的提示资源，不执行第三方代码。
package main

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type resource struct {
	SourceID      string `json:"source_id"`
	Path          string `json:"path"`
	Family        string `json:"family"`
	Body          string `json:"body"`
	SHA256        string `json:"sha256"`
	License       string `json:"license"`
	LicenseText   string `json:"license_text"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

func main() {
	input := flag.String("input", "dist/qa-integration/archives", "已下载源码包目录")
	output := flag.String("output", "internal/armor/assets/library.json", "资源清单路径")
	flag.Parse()
	var all []resource
	for _, spec := range []struct{ id, root string }{{"calrton", "jailbreak-prompts-main/"}, {"gpt-instruct", "gpt-instruct-main/"}, {"dsh-purge", "dsh-purge-master/"}} {
		raw, err := os.ReadFile(filepath.Join(*input, spec.id+".zip"))
		must(err)
		reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		must(err)
		sum := sha256.Sum256(raw)
		archiveSHA := hex.EncodeToString(sum[:])
		license := string(readEntry(reader, spec.root+"LICENSE"))
		add := func(path, family string, body []byte) {
			h := sha256.Sum256(body)
			all = append(all, resource{spec.id, path, family, string(body), hex.EncodeToString(h[:]), "MIT（仓库声明，引用文本另见原始来源）", license, archiveSHA})
		}
		switch spec.id {
		case "calrton":
			for _, f := range reader.File {
				name := strings.TrimPrefix(f.Name, spec.root)
				if strings.Contains(name, "/") || name == "README.md" || !(strings.HasSuffix(name, ".mkd") || strings.HasSuffix(name, ".md")) {
					continue
				}
				family := "gpt"
				for key, value := range map[string]string{"ANTHROPIC": "claude", "GOOGLE": "gemini", "DEEPSEEK": "deepseek", "GROK": "grok", "META": "llama", "NOUS": "nous", "MICROSOFT": "copilot", "PERPLEXITY": "perplexity", "BRAVE": "brave", "CURSOR": "cursor"} {
					if strings.Contains(name, key) {
						family = value
					}
				}
				add(name, family, readEntry(reader, f.Name))
			}
		case "gpt-instruct":
			for _, name := range []string{"gpt-5.6-sol-v45", "gpt-6-astra-v2-rc1", "gpt-6.1-sol-v1-rc2"} {
				packed := readEntry(reader, spec.root+name+".zip")
				nested, err := zip.NewReader(bytes.NewReader(packed), int64(len(packed)))
				must(err)
				file := name + ".md"
				if name == "gpt-5.6-sol-v45" {
					file = "gpt-5.6-sol-unrestricted-v45.md"
				}
				add(name+".zip!"+file, "gpt", readEntry(nested, file))
			}
		case "dsh-purge":
			// 严格读取 JSON 字面量，按原项目公开的数据格式解包，绝不求值 JS。
			table := string(readEntry(reader, spec.root+"lib/asset-table.js"))
			var parts []string
			match := regexp.MustCompile(`(?s)const SLOT_ROWS = (\[.*?\]);`).FindStringSubmatch(table)
			if len(match) != 2 {
				panic("dsh-purge 资源格式变化，请检查")
			}
			must(json.Unmarshal([]byte(match[1]), &parts))
			value := func(name string) []byte {
				m := regexp.MustCompile(`export const ` + name + ` = "([A-Za-z0-9+/=]+)";`).FindStringSubmatch(table)
				if len(m) != 2 {
					panic("资源字段缺失")
				}
				b, err := base64.StdEncoding.DecodeString(m[1])
				must(err)
				return b
			}
			mask := base64.StdEncoding.EncodeToString(value("MASK"))
			key := sha256.Sum256([]byte(strings.Join([]string{"cordis.slot.v2", "dsh-purge", "surface:web|desktop", "ov.table.1", mask}, "\x1e")))
			block, err := aes.NewCipher(key[:])
			must(err)
			gcm, err := cipher.NewGCM(block)
			must(err)
			sealed, err := base64.StdEncoding.DecodeString(strings.Join(parts, ""))
			must(err)
			sealed = append(sealed, value("TAG")...)
			compressed, err := gcm.Open(nil, value("IV"), sealed, nil)
			must(err)
			zr, err := zlib.NewReader(bytes.NewReader(compressed))
			must(err)
			body, err := io.ReadAll(io.LimitReader(zr, 1<<20))
			zr.Close()
			must(err)
			add("lib/asset-table.js!default-prompt", "generic", body)
		}
	}
	if cold, err := os.ReadFile(filepath.Join(*input, "coldbrew-resources.json")); err == nil {
		var files []resource
		must(json.Unmarshal(cold, &files))
		all = append(all, files...)
	}
	encoded, err := json.MarshalIndent(all, "", "  ")
	must(err)
	must(os.MkdirAll(filepath.Dir(*output), 0o755))
	must(os.WriteFile(*output, encoded, 0o644))
	fmt.Printf("已打包 %d 个提示资源；输出 %s\n", len(all), *output)
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func readEntry(z *zip.Reader, name string) []byte {
	for _, f := range z.File {
		if f.Name == name {
			if f.UncompressedSize64 > 2<<20 {
				panic("资源文件过大")
			}
			reader, err := f.Open()
			must(err)
			raw, err := io.ReadAll(io.LimitReader(reader, 2<<20))
			reader.Close()
			must(err)
			return raw
		}
	}
	panic("源文件不存在：" + name)
}
