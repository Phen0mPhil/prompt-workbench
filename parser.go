package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ledongthuc/pdf"
)

func ExtractTextFromPath(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	return ExtractText(filepath.Base(filePath), data)
}

func ExtractText(filename string, data []byte) (string, error) {
	lowerName := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lowerName, ".pdf"):
		return parsePDF(data)
	case strings.HasSuffix(lowerName, ".docx"):
		return parseDocx(data)
	default:
		return string(data), nil
	}
}

func parsePDF(data []byte) (string, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var textBuilder strings.Builder
	b, err := reader.GetPlainText()
	if err != nil {
		return "", err
	}
	_, err = io.Copy(&textBuilder, b)
	return textBuilder.String(), err
}

func parseDocx(data []byte) (string, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	for _, f := range zipReader.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			content, _ := io.ReadAll(rc)
			rc.Close()
			return stripXMLTags(string(content)), nil
		}
	}
	return "", nil
}

func stripXMLTags(xml string) string {
	var result strings.Builder
	inTag := false
	for _, char := range xml {
		if char == '<' {
			inTag = true
			continue
		}
		if char == '>' {
			inTag = false
			result.WriteRune(' ')
			continue
		}
		if !inTag {
			result.WriteRune(char)
		}
	}
	return strings.TrimSpace(result.String())
}
