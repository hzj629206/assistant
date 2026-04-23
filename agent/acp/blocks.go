package acp

import "strings"

// SystemBlock builds one ACP text content block annotated as a system role.
func SystemBlock(prompts ...string) ContentBlock {
	parts := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		if trimmed := strings.TrimSpace(prompt); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return ContentBlock{
		"type": "text",
		"text": strings.Join(parts, "\n\n"),
		"annotations": map[string]any{
			"role": "system",
		},
	}
}

// TextBlock builds one ACP text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{
		"type": "text",
		"text": text,
	}
}

// ImageBlock builds one ACP image content block.
func ImageBlock(mimeType string, dataBase64 string, uri string) ContentBlock {
	block := ContentBlock{
		"type":     "image",
		"mimeType": mimeType,
		"data":     dataBase64,
	}
	if uri != "" {
		block["uri"] = uri
	}
	return block
}

// ResourceLinkBlock builds one ACP resource_link content block.
func ResourceLinkBlock(uri string, name string, mimeType string, size int64) ContentBlock {
	block := ContentBlock{
		"type": "resource_link",
		"uri":  uri,
		"name": name,
	}
	if mimeType != "" {
		block["mimeType"] = mimeType
	}
	if size > 0 {
		block["size"] = size
	}
	return block
}

// EmbeddedResourceBlock builds one ACP embedded resource content block.
func EmbeddedResourceBlock(uri string, mimeType string, text string) ContentBlock {
	resource := map[string]any{
		"uri":  uri,
		"text": text,
	}
	if mimeType != "" {
		resource["mimeType"] = mimeType
	}
	return ContentBlock{
		"type":     "resource",
		"resource": resource,
	}
}
