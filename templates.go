package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed templates/*.md
var embeddedTemplates embed.FS

// writeDefaultTemplates 把内置模板复制到数据目录（已存在的不覆盖，用户可自行修改）。
func writeDefaultTemplates(root string) error {
	dir := templatesDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := embeddedTemplates.ReadDir("templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		dst := filepath.Join(dir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := embeddedTemplates.ReadFile("templates/" + e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// loadTemplate 优先读数据目录里（可能被用户改过的）模板，退回内置版本。
func loadTemplate(root, name string) (string, error) {
	path := filepath.Join(templatesDir(root), name+".md")
	if data, err := os.ReadFile(path); err == nil {
		return string(data), nil
	}
	data, err := embeddedTemplates.ReadFile("templates/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("找不到模板 %s: %w", name, err)
	}
	return string(data), nil
}

func renderTemplate(tpl string, vars map[string]string) string {
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}
