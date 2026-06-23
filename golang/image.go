package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var imageDir = "../public/image"

const initialPostMaxID = 10000

func cleanupGeneratedImageFiles() {
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		dot := strings.IndexByte(name, '.')
		if dot <= 0 {
			continue
		}
		id, err := strconv.Atoi(name[:dot])
		if err != nil || id <= initialPostMaxID {
			continue
		}
		_ = os.Remove(filepath.Join(imageDir, name))
	}
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func imageExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}

func imageFilePath(id int, mime string) string {
	return filepath.Join(imageDir, strconv.Itoa(id)+imageExt(mime))
}

func writeImageFile(id int, mime string, imgdata []byte) error {
	if imageExt(mime) == "" {
		return fmt.Errorf("unknown image mime: %s", mime)
	}
	if err := os.MkdirAll(imageDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(imageFilePath(id, mime), imgdata, 0644)
}

func exportImages(ctx context.Context) error {
	if envImageDir := os.Getenv("ISUCONP_IMAGE_DIR"); envImageDir != "" {
		imageDir = envImageDir
	}
	if err := os.MkdirAll(imageDir, 0755); err != nil {
		return err
	}
	cleanupGeneratedImageFiles()

	markerPath := filepath.Join(imageDir, ".exported")
	if _, err := os.Stat(markerPath); err == nil {
		return nil
	}

	rows, err := db.QueryxContext(ctx, "SELECT `id`, `mime`, `imgdata` FROM `posts`")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		p := Post{}
		if err := rows.StructScan(&p); err != nil {
			return err
		}
		if err := writeImageFile(p.ID, p.Mime, p.Imgdata); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return os.WriteFile(markerPath, []byte(time.Now().Format(time.RFC3339)), 0644)
}
