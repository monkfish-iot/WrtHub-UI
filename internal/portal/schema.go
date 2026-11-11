// schema.go PORTAL 模板属性规范（schema.json）：
// 模板包内可放置 schema.json 声明"哪些属性可在线编辑"，编辑页据此渲染可视化表单；
// 配置值以 JSON 存于 portal_templates.config_json（字段随模板变化，无需改表结构）。
//
// schema.json 结构：
//
//	{
//	  "version": 1,
//	  "pages": [{
//	    "id": "login", "name": "登录页", "file": "index.html",
//	    "fields": [
//	      {"key":"topic","label":"主标题","type":"text","default":"欢迎","maxlength":64},
//	      {"key":"bg_color","label":"背景色","type":"color","default":"#2563eb"},
//	      {"key":"bg_image","label":"背景图","type":"image"}
//	    ]
//	  }]
//	}
//
// 字段类型：text（文本）/ color（#hex 颜色，空=模板内置默认）/ image（包内图片相对路径，空=无）。
package portal

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// schema 规模与取值限制。
const (
	MaxSchemaPages  = 8
	MaxSchemaFields = 32 // 每页字段数上限
	MaxSchemaValLen = 512
	SchemaFileName  = "schema.json"
)

var (
	schemaKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	colorRe     = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
)

// SchemaField 单个可编辑属性。
type SchemaField struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Type      string `json:"type"` // text / color / image
	Default   string `json:"default"`
	MaxLength int    `json:"maxlength"`
	Help      string `json:"help"`
}

// SchemaPage 一个页面（对应一个 html 文件）的属性组。
type SchemaPage struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	File   string        `json:"file"` // 预览对应的模板内 html
	Fields []SchemaField `json:"fields"`
}

// TemplateSchema 模板属性规范。
type TemplateSchema struct {
	Version int          `json:"version"`
	Pages   []SchemaPage `json:"pages"`
}

// imageExts 模板包内可作为 image 字段值的扩展名。
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".svg": true, ".ico": true, ".webp": true,
}

// IsImageExt 判断文件是否为图片资源。
func IsImageExt(name string) bool {
	return imageExts[strings.ToLower(path.Ext(name))]
}

// ParseSchema 解析并校验 schema.json 内容。
func ParseSchema(data []byte) (*TemplateSchema, error) {
	// 兼容 Windows 编辑器保存的 UTF-8 BOM（否则 json 解析报 invalid character 'ï'）。
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var s TemplateSchema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, bad("schema.json 不是合法 JSON：" + err.Error())
	}
	if s.Version != 1 {
		return nil, bad("schema.json version 仅支持 1")
	}
	if len(s.Pages) == 0 || len(s.Pages) > MaxSchemaPages {
		return nil, bad("schema.json pages 数量须在 1-8 之间")
	}
	seen := map[string]bool{}
	for i := range s.Pages {
		p := &s.Pages[i]
		if p.ID == "" || p.Name == "" || p.File == "" {
			return nil, bad("schema.json 每个 page 须含 id/name/file")
		}
		file, err := CleanRelPath(p.File)
		if err != nil {
			return nil, bad("schema.json page.file 非法：" + p.File)
		}
		if ext := strings.ToLower(path.Ext(file)); ext != ".html" && ext != ".htm" {
			return nil, bad("schema.json page.file 须为 html 文件：" + p.File)
		}
		p.File = file
		if len(p.Fields) == 0 || len(p.Fields) > MaxSchemaFields {
			return nil, bad("schema.json 每页字段数须在 1-32 之间")
		}
		for j := range p.Fields {
			f := &p.Fields[j]
			if !schemaKeyRe.MatchString(f.Key) {
				return nil, bad("schema.json 字段 key 非法（须小写字母开头的蛇形命名）：" + f.Key)
			}
			if seen[f.Key] {
				return nil, bad("schema.json 字段 key 重复：" + f.Key)
			}
			seen[f.Key] = true
			switch f.Type {
			case "text", "color", "image":
			default:
				return nil, bad("schema.json 字段 " + f.Key + " type 仅支持 text/color/image")
			}
			if f.Label == "" {
				return nil, bad("schema.json 字段 " + f.Key + " 缺少 label")
			}
			if f.MaxLength <= 0 || f.MaxLength > MaxSchemaValLen {
				f.MaxLength = MaxSchemaValLen
			}
			if f.Type == "color" && f.Default != "" && !colorRe.MatchString(f.Default) {
				return nil, bad("schema.json 字段 " + f.Key + " default 须为 #hex 颜色")
			}
			if len(f.Default) > MaxSchemaValLen {
				return nil, bad("schema.json 字段 " + f.Key + " default 过长")
			}
		}
	}
	return &s, nil
}

// ReadSchema 读取模板包内 schema.json；文件不存在返回 (nil, nil)，存在但非法返回错误。
func (s *Store) ReadSchema(code string) (*TemplateSchema, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir(code), SchemaFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseSchema(data)
}

// Validate 按 schema 清洗配置值：仅接受已声明 key，按类型校验；
// 空值不写入（语义=使用模板内置默认）。hasImage 判断图片路径是否为包内文件。
func (s *TemplateSchema) Validate(values map[string]string, hasImage func(string) bool) (map[string]string, error) {
	out := make(map[string]string, 16)
	for i := range s.Pages {
		for j := range s.Pages[i].Fields {
			f := &s.Pages[i].Fields[j]
			v := strings.TrimSpace(values[f.Key])
			if v == "" {
				continue
			}
			switch f.Type {
			case "text":
				if len(v) > f.MaxLength {
					return nil, bad("字段「" + f.Label + "」超过长度限制")
				}
			case "color":
				if !colorRe.MatchString(v) {
					return nil, bad("字段「" + f.Label + "」须为 #hex 颜色值")
				}
			case "image":
				if _, err := CleanRelPath(v); err != nil || !IsImageExt(v) || hasImage == nil || !hasImage(v) {
					return nil, bad("字段「" + f.Label + "」须为模板包内的图片文件")
				}
			}
			out[f.Key] = v
		}
	}
	return out, nil
}
