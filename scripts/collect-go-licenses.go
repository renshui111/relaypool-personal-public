//go:build ignore

// 从实际构建工具链与模块缓存收集发行许可，不把缓存内容当成项目源码发布。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type module struct {
	Path, Version, Dir, Sum string
	Main                    bool
}
type entry struct {
	Module, Version, Sum string
	Files                []string
}

func main() {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		panic("请指定许可输出目录")
	}
	dest := os.Args[1]
	target := "./cmd/server"
	if len(os.Args) == 3 {
		target = os.Args[2]
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		panic(err)
	}
	modules := []module{}
	seen := map[string]bool{}
	for _, targetOS := range []string{"linux", "windows"} {
		cmd := exec.Command("go", "list", "-deps", "-json", target)
		cmd.Env = append(os.Environ(), "GOOS="+targetOS, "GOARCH=amd64", "CGO_ENABLED=0")
		raw, err := cmd.Output()
		if err != nil {
			panic(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		for {
			var p struct{ Module *module }
			err := dec.Decode(&p)
			if err == io.EOF {
				break
			}
			if err != nil {
				panic(err)
			}
			if p.Module != nil && !p.Module.Main && !seen[p.Module.Path] {
				seen[p.Module.Path] = true
				modules = append(modules, *p.Module)
			}
		}
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Path < modules[j].Path })
	root, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		panic(err)
	}
	modules = append(modules, module{Path: "go-runtime", Version: strings.TrimSpace(command("go", "version")), Dir: strings.TrimSpace(string(root))})
	entries := []entry{}
	for _, m := range modules {
		item := entry{Module: m.Path, Version: m.Version, Sum: m.Sum, Files: []string{}}
		folder := filepath.Join(dest, strings.NewReplacer("/", "_", "\\", "_").Replace(m.Path)+"@"+strings.ReplaceAll(m.Version, " ", "_"))
		if err := os.MkdirAll(folder, 0755); err != nil {
			panic(err)
		}
		err := filepath.WalkDir(m.Dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if path != m.Dir && m.Path == "go-runtime" {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			base := strings.ToUpper(d.Name())
			if !strings.HasPrefix(base, "LICENSE") && !strings.HasPrefix(base, "COPYING") && base != "NOTICE" && base != "NOTICE.TXT" && base != "PATENTS" {
				return nil
			}
			rel, err := filepath.Rel(m.Dir, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if len(data) > 4<<20 {
				return fmt.Errorf("许可文件过大：%s", rel)
			}
			target := filepath.Join(folder, rel)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(target, data, 0644); err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			item.Files = append(item.Files, filepath.ToSlash(rel)+" sha256:"+hex.EncodeToString(sum[:]))
			return nil
		})
		if err != nil {
			panic(err)
		}
		if len(item.Files) == 0 {
			panic("模块没有可收集的许可文件：" + m.Path)
		}
		entries = append(entries, item)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "modules.json"), append(data, '\n'), 0644); err != nil {
		panic(err)
	}
	fmt.Printf("已收集 %d 个 Go 模块及工具链的许可\n", len(entries))
}

func command(name string, args ...string) string {
	raw, err := exec.Command(name, args...).Output()
	if err != nil {
		panic(err)
	}
	return string(raw)
}
