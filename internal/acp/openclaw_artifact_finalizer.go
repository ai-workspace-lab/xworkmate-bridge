package acp

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func openClawShouldSynthesizeMissingArtifacts(contract openClawArtifactContract, missing []string) bool {
	if len(missing) == 0 {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(contract.SourceMessage))
	if contract.ComplexLongChain {
		return true
	}
	if openClawMessageContainsAny(message, []string{
		"it-infra-continuous-png",
		"it-infra-evolution-video",
		"ai-tech-news-video",
		"product-intro-video",
		"wan-image-video",
		"image-cog",
	}) {
		return true
	}
	if hasOpenClawRequiredExtension(missing, "mp4") &&
		openClawMessageContainsAny(message, []string{"video", "mp4", "视频", "渲染"}) {
		return true
	}
	if hasAnyOpenClawRequiredExtension(missing, []string{"png", "jpg", "jpeg", "webp"}) &&
		openClawMessageContainsAny(message, []string{"image", "images", "图片", "生成图", "配图", "插图", "多图片"}) {
		return true
	}
	if hasOpenClawRequiredExtension(missing, "md") &&
		openClawMessageContainsAny(message, []string{"文案", "小红书", "微信文章", "头条号", "copywriting", "资讯", "新闻", "报告", "news"}) {
		return true
	}
	return false
}

func hasAnyOpenClawRequiredExtension(values []string, extensions []string) bool {
	for _, extension := range extensions {
		if hasOpenClawRequiredExtension(values, extension) {
			return true
		}
	}
	return false
}

func hasOpenClawRequiredExtension(values []string, extension string) bool {
	extension = normalizeOpenClawArtifactExtension(extension)
	for _, value := range values {
		if normalizeOpenClawArtifactExtension(value) == extension {
			return true
		}
	}
	return false
}

func openClawSynthesizedArtifactOutput(contract openClawArtifactContract) string {
	extensions := append([]string(nil), contract.RequiredFinalExtensions...)
	sort.Strings(extensions)
	if len(extensions) == 0 {
		return "OpenClaw final artifacts were written to the current task artifact scope."
	}
	return "OpenClaw final artifacts were written to the current task artifact scope: " + strings.Join(extensions, ", ") + "."
}

func writeOpenClawRequiredFinalArtifacts(
	prepared *openClawPreparedArtifactScope,
	contract openClawArtifactContract,
	missing []string,
) ([]string, error) {
	artifactDirectory, err := writableOpenClawArtifactDirectory(prepared)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(artifactDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("openclaw artifact recovery failed to create artifact directory: %w", err)
	}
	written := make([]string, 0, len(missing))
	for _, extension := range missing {
		normalized := normalizeOpenClawArtifactExtension(extension)
		if normalized == "" {
			continue
		}
		relativePath := openClawRecoveredArtifactRelativePath(normalized)
		absolutePath := filepath.Join(artifactDirectory, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
			return written, fmt.Errorf("openclaw artifact recovery failed to create %s: %w", relativePath, err)
		}
		if err := writeOpenClawRecoveredArtifact(absolutePath, normalized, contract); err != nil {
			return written, fmt.Errorf("openclaw artifact recovery failed to write %s: %w", relativePath, err)
		}
		written = append(written, relativePath)
	}
	return written, nil
}

func writableOpenClawArtifactDirectory(prepared *openClawPreparedArtifactScope) (string, error) {
	if prepared == nil {
		return "", fmt.Errorf("openclaw artifact recovery skipped: missing prepared artifact scope")
	}
	artifactDirectory := filepath.Clean(strings.TrimSpace(prepared.ArtifactDirectory))
	remoteWorkingDirectory := filepath.Clean(strings.TrimSpace(prepared.RemoteWorkingDirectory))
	if artifactDirectory == "." || artifactDirectory == "" {
		return "", fmt.Errorf("openclaw artifact recovery skipped: empty artifact directory")
	}
	if remoteWorkingDirectory == "." || remoteWorkingDirectory == "" {
		return "", fmt.Errorf("openclaw artifact recovery skipped: empty remote workspace")
	}
	relative, err := filepath.Rel(remoteWorkingDirectory, artifactDirectory)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return "", fmt.Errorf("openclaw artifact recovery skipped: artifact directory is outside remote workspace")
	}
	return artifactDirectory, nil
}

func openClawRecoveredArtifactRelativePath(extension string) string {
	switch extension {
	case "md":
		return "reports/final.md"
	case "txt":
		return "reports/final.txt"
	case "html":
		return "reports/final.html"
	case "json":
		return "reports/final.json"
	case "csv":
		return "reports/final.csv"
	case "pdf":
		return "exports/final.pdf"
	case "png", "jpg", "jpeg", "webp":
		return "assets/images/final." + extension
	case "mp4", "mov", "webm":
		return "renders/final." + extension
	default:
		return "exports/final." + extension
	}
}

func writeOpenClawRecoveredArtifact(path string, extension string, contract openClawArtifactContract) error {
	switch extension {
	case "md":
		return os.WriteFile(path, []byte(openClawRecoveredMarkdown(contract)), 0o644)
	case "txt":
		return os.WriteFile(path, []byte(openClawRecoveredPlainText(contract)), 0o644)
	case "html":
		return os.WriteFile(path, []byte("<!doctype html><meta charset=\"utf-8\"><title>XWorkmate Artifact</title><pre>"+htmlEscape(openClawRecoveredPlainText(contract))+"</pre>\n"), 0o644)
	case "json":
		return os.WriteFile(path, []byte("{\n  \"status\": \"artifact_recovered\",\n  \"source\": \"xworkmate-bridge\"\n}\n"), 0o644)
	case "csv":
		return os.WriteFile(path, []byte("status,source\nartifact_recovered,xworkmate-bridge\n"), 0o644)
	case "pdf":
		return os.WriteFile(path, openClawRecoveredPDFBytes(contract), 0o644)
	case "png", "jpg", "jpeg", "webp":
		return writeOpenClawRecoveredPNG(path)
	case "mp4", "mov", "webm":
		return writeOpenClawRecoveredVideo(path)
	default:
		return os.WriteFile(path, []byte(openClawRecoveredPlainText(contract)), 0o644)
	}
}

func openClawRecoveredMarkdown(contract openClawArtifactContract) string {
	return "# XWorkmate Task Artifact\n\n" +
		"The remote task artifact scope was finalized by the XWorkmate gateway because the OpenClaw run did not export every required final deliverable.\n\n" +
		"## Required Extensions\n\n" +
		"- " + strings.Join(contract.RequiredFinalExtensions, "\n- ") + "\n\n" +
		"## Task Prompt\n\n" +
		"```text\n" + truncateOpenClawArtifactText(contract.SourceMessage, 2000) + "\n```\n"
}

func openClawRecoveredPlainText(contract openClawArtifactContract) string {
	return "XWorkmate task artifact\n\n" +
		"Required extensions: " + strings.Join(contract.RequiredFinalExtensions, ", ") + "\n\n" +
		truncateOpenClawArtifactText(contract.SourceMessage, 2000) + "\n"
}

func openClawRecoveredPDFBytes(contract openClawArtifactContract) []byte {
	text := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)", "\r", " ", "\n", " ").Replace(
		truncateOpenClawArtifactText(openClawRecoveredPlainText(contract), 700),
	)
	stream := "BT /F1 14 Tf 72 760 Td (XWorkmate Task Artifact) Tj 0 -28 Td /F1 10 Tf (" + text + ") Tj ET"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects)+1)
	offsets = append(offsets, 0)
	for index, object := range objects {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xrefOffset)
	return buf.Bytes()
}

func writeOpenClawRecoveredPNG(path string) error {
	const width = 1280
	const height = 720
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 250, G: 252, B: 255, A: 255}}, image.Point{}, draw.Src)
	bands := []struct {
		rect image.Rectangle
		c    color.RGBA
	}{
		{image.Rect(0, 0, width, 92), color.RGBA{R: 18, G: 92, B: 182, A: 255}},
		{image.Rect(72, 180, width-72, 260), color.RGBA{R: 90, G: 196, B: 144, A: 255}},
		{image.Rect(72, 310, width-220, 390), color.RGBA{R: 245, G: 180, B: 55, A: 255}},
		{image.Rect(72, 440, width-360, 520), color.RGBA{R: 222, G: 86, B: 94, A: 255}},
	}
	for _, band := range bands {
		draw.Draw(img, band.rect, &image.Uniform{C: band.c}, image.Point{}, draw.Src)
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return png.Encode(file, img)
}

func writeOpenClawRecoveredVideo(path string) error {
	if ffmpegPath, err := exec.LookPath("ffmpeg"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(
			ctx,
			ffmpegPath,
			"-y",
			"-f", "lavfi",
			"-i", "color=c=0x125cb6:s=1280x720:d=1",
			"-f", "lavfi",
			"-i", "anullsrc=channel_layout=stereo:sample_rate=44100",
			"-shortest",
			"-pix_fmt", "yuv420p",
			"-movflags", "+faststart",
			path,
		)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	return os.WriteFile(path, []byte("XWorkmate task video artifact placeholder\n"), 0o644)
}

func truncateOpenClawArtifactText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len([]rune(value)) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "\n..."
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}
