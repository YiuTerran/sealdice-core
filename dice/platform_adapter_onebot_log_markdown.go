package dice

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	htmlrenderer "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var onebotBridgeBotMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(
		htmlrenderer.WithHardWraps(),
		renderer.WithNodeRenderers(util.Prioritized(&onebotBridgeSafeMarkdownNodeRenderer{}, 100)),
	),
)

func (w *onebotBridgeLogSnapshotWriter) renderBotMarkdownBody(body string, display *onebotBridgeLogDisplay) (string, error) {
	if len(body) > onebotBridgeArtifactMaxBytes {
		return "", errOnebotBridgeArtifactTooLarge
	}
	source := onebotBridgeNormalizeMarkdownLineBreaks(body)
	document := onebotBridgeBotMarkdown.Parser().Parse(text.NewReader(source))
	if err := w.resolveBotMarkdownMentions(document, source, display); err != nil {
		return "", err
	}

	out := &onebotBridgeLimitedBuffer{}
	if _, err := out.WriteString("<div>"); err != nil {
		return "", err
	}
	lineWriter := &onebotBridgeMarkdownLineWriter{out: out}
	renderErr := onebotBridgeBotMarkdown.Renderer().Render(lineWriter, source, document)
	if lineWriter.err != nil {
		return "", lineWriter.err
	}
	if renderErr != nil {
		return "", renderErr
	}
	if _, err := out.WriteString("</div>"); err != nil {
		return "", err
	}
	return out.String(), nil
}

func onebotBridgeNormalizeMarkdownLineBreaks(value string) []byte {
	result := make([]byte, 0, len(value))
	for index := range len(value) {
		if value[index] == '\n' && index > 0 && value[index-1] == '\r' {
			continue
		}
		if value[index] == '\r' {
			result = append(result, '\n')
			continue
		}
		result = append(result, value[index])
	}
	return result
}

func (w *onebotBridgeLogSnapshotWriter) resolveBotMarkdownMentions(document ast.Node, source []byte, display *onebotBridgeLogDisplay) error {
	var nodes []ast.Node
	var textRuns [][]*ast.Text
	mentionBudget := onebotBridgeArtifactMaxBytes
	err := ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.CodeSpan, *ast.CodeBlock, *ast.FencedCodeBlock, *ast.HTMLBlock:
			return ast.WalkSkipChildren, nil
		case *ast.Link:
			if _, isMention := onebotBridgeMarkdownLinkMention(node, source); isMention {
				nodes = append(nodes, node)
				return ast.WalkSkipChildren, nil
			}
		}
		for child := node.FirstChild(); child != nil; {
			first, ok := child.(*ast.Text)
			if !ok {
				child = child.NextSibling()
				continue
			}
			run := []*ast.Text{first}
			previous := first
			next := first.NextSibling()
			for {
				current, ok := next.(*ast.Text)
				if !ok || !onebotBridgeMarkdownTextRunContinues(previous, current) {
					break
				}
				run = append(run, current)
				previous = current
				next = current.NextSibling()
			}
			textRuns = append(textRuns, run)
			child = next
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return err
	}
	for _, node := range nodes {
		switch node := node.(type) {
		case *ast.Link:
			token, ok := onebotBridgeMarkdownLinkMention(node, source)
			if !ok {
				continue
			}
			literal, err := w.resolveBotMarkdownMention(token, display, &mentionBudget)
			if err != nil {
				return err
			}
			node.Parent().ReplaceChild(node.Parent(), node, literal)
		}
	}
	for _, run := range textRuns {
		if err := w.resolveBotMarkdownTextRun(run, source, display, &mentionBudget); err != nil {
			return err
		}
	}
	return nil
}

func onebotBridgeMarkdownTextRunContinues(previous, current *ast.Text) bool {
	return previous.Segment.Padding == 0 && current.Segment.Padding == 0 &&
		previous.Segment.Stop == current.Segment.Start && !previous.SoftLineBreak() && !previous.HardLineBreak() &&
		previous.IsRaw() == current.IsRaw()
}

func (w *onebotBridgeLogSnapshotWriter) resolveBotMarkdownMention(token onebotBridgeLogMentionToken, display *onebotBridgeLogDisplay, budget *int) (*ast.String, error) {
	if *budget <= 0 {
		return nil, errOnebotBridgeArtifactTooLarge
	}
	name, err := w.resolveMention(token, display)
	if err != nil {
		return nil, err
	}
	literalLength := len(name) + 1
	if literalLength > *budget {
		return nil, errOnebotBridgeArtifactTooLarge
	}
	*budget -= literalLength
	literal := ast.NewString([]byte("@" + name))
	literal.SetRaw(true)
	return literal, nil
}

func onebotBridgeMarkdownLinkMention(node *ast.Link, source []byte) (onebotBridgeLogMentionToken, bool) {
	destination := string(node.Destination)
	if !strings.HasPrefix(destination, "mqqapi://markdown/mention?") {
		return onebotBridgeLogMentionToken{}, false
	}
	target, ok := onebotBridgeMarkdownTarget(destination)
	label := onebotBridgeMarkdownLinkLabel(node, source)
	if !ok {
		digest := sha256.Sum256([]byte(destination))
		target = fmt.Sprintf("opaque:%x", digest[:8])
		label = ""
	} else if !validOnebotBridgeLogDisplayName(label) || !strings.HasPrefix(label, "@") || label == "@" {
		label = ""
	}
	return onebotBridgeLogMentionToken{aliases: []string{target}, label: label}, true
}

func onebotBridgeMarkdownLinkLabel(node *ast.Link, source []byte) string {
	const maxLabelBytes = 256
	label := make([]byte, 0, maxLabelBytes+1)
	appendValue := func(value []byte) {
		remaining := maxLabelBytes + 1 - len(label)
		if remaining <= 0 {
			return
		}
		if len(value) > remaining {
			value = value[:remaining]
		}
		label = append(label, value...)
	}
	var visit func(ast.Node)
	visit = func(current ast.Node) {
		if len(label) > maxLabelBytes {
			return
		}
		switch current := current.(type) {
		case *ast.Text:
			appendValue(current.Value(source))
		case *ast.String:
			appendValue(current.Value)
		case *ast.RawHTML:
			for index := range current.Segments.Len() {
				segment := current.Segments.At(index)
				appendValue(segment.Value(source))
			}
		case *ast.AutoLink:
			appendValue(current.Label(source))
		default:
			for child := current.FirstChild(); child != nil; child = child.NextSibling() {
				visit(child)
				if len(label) > maxLabelBytes {
					return
				}
			}
		}
	}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		visit(child)
		if len(label) > maxLabelBytes {
			return ""
		}
	}
	return string(label)
}

func (w *onebotBridgeLogSnapshotWriter) resolveBotMarkdownTextRun(nodes []*ast.Text, source []byte, display *onebotBridgeLogDisplay, budget *int) error {
	var totalLength int
	for _, node := range nodes {
		totalLength += len(node.Value(source))
	}
	value := make([]byte, 0, totalLength)
	for _, node := range nodes {
		value = append(value, node.Value(source)...)
	}
	tokens := onebotBridgeParseMentionTokens(string(value))
	if len(tokens) == 0 {
		return nil
	}
	pieces := make([]ast.Node, 0, len(tokens)*2+2)
	last := 0
	raw := nodes[0].IsRaw()
	for _, token := range tokens {
		if token.start < last || token.end > len(value) {
			continue
		}
		if token.start > last {
			pieces = append(pieces, onebotBridgeMarkdownLiteral(value[last:token.start], raw))
		}
		mention, err := w.resolveBotMarkdownMention(token, display, budget)
		if err != nil {
			return err
		}
		pieces = append(pieces, mention)
		last = token.end
	}
	if last < len(value) {
		pieces = append(pieces, onebotBridgeMarkdownLiteral(value[last:], raw))
	}
	if len(pieces) == 0 {
		return nil
	}
	lastNode := nodes[len(nodes)-1]
	if lastNode.SoftLineBreak() || lastNode.HardLineBreak() {
		breakNode := ast.NewTextSegment(text.NewSegment(0, 0))
		breakNode.SetRaw(lastNode.IsRaw())
		breakNode.SetSoftLineBreak(lastNode.SoftLineBreak())
		breakNode.SetHardLineBreak(lastNode.HardLineBreak())
		pieces = append(pieces, breakNode)
	}
	markdownNodes := make([]ast.Node, len(nodes))
	for index, node := range nodes {
		markdownNodes[index] = node
	}
	onebotBridgeReplaceMarkdownNodes(markdownNodes, pieces)
	return nil
}

func onebotBridgeMarkdownLiteral(value []byte, raw bool) *ast.String {
	literal := ast.NewString(append([]byte(nil), value...))
	literal.SetRaw(raw)
	return literal
}

func onebotBridgeReplaceMarkdownNodes(nodes []ast.Node, replacements []ast.Node) {
	parent := nodes[0].Parent()
	next := nodes[len(nodes)-1].NextSibling()
	for _, node := range nodes {
		parent.RemoveChild(parent, node)
	}
	for _, replacement := range replacements {
		parent.InsertBefore(parent, next, replacement)
	}
}

type onebotBridgeMarkdownLineWriter struct {
	out *onebotBridgeLimitedBuffer
	err error
}

func (w *onebotBridgeMarkdownLineWriter) Write(value []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	start := 0
	for index, char := range value {
		var entity string
		switch char {
		case '\n':
			entity = "&#10;"
		case '\r':
			entity = "&#13;"
		default:
			continue
		}
		if _, err := w.out.Write(value[start:index]); err != nil {
			w.err = err
			return index, err
		}
		if _, err := w.out.WriteString(entity); err != nil {
			w.err = err
			return index + 1, err
		}
		start = index + 1
	}
	if _, err := w.out.Write(value[start:]); err != nil {
		w.err = err
		return start, err
	}
	return len(value), nil
}

type onebotBridgeSafeMarkdownNodeRenderer struct{}

func (*onebotBridgeSafeMarkdownNodeRenderer) RegisterFuncs(register renderer.NodeRendererFuncRegisterer) {
	register.Register(ast.KindHTMLBlock, onebotBridgeRenderEscapedHTMLBlock)
	register.Register(ast.KindRawHTML, onebotBridgeRenderEscapedRawHTML)
	register.Register(ast.KindImage, onebotBridgeRenderImagePlaceholder)
}

func onebotBridgeRenderEscapedRawHTML(writer util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		if _, err := writer.WriteString(`<span style="white-space:pre-wrap">`); err != nil {
			return ast.WalkSkipChildren, err
		}
		for index := range node.(*ast.RawHTML).Segments.Len() {
			segment := node.(*ast.RawHTML).Segments.At(index)
			if err := onebotBridgeWriteEscapedMarkdownBytes(writer, segment.Value(source)); err != nil {
				return ast.WalkSkipChildren, err
			}
		}
		return ast.WalkSkipChildren, nil
	}
	if _, err := writer.WriteString("</span>"); err != nil {
		return ast.WalkContinue, err
	}
	return ast.WalkContinue, nil
}

func onebotBridgeRenderEscapedHTMLBlock(writer util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	block := node.(*ast.HTMLBlock)
	if entering {
		if _, err := writer.WriteString("<pre>"); err != nil {
			return ast.WalkSkipChildren, err
		}
		for index := range block.Lines().Len() {
			line := block.Lines().At(index)
			if err := onebotBridgeWriteEscapedMarkdownBytes(writer, line.Value(source)); err != nil {
				return ast.WalkSkipChildren, err
			}
		}
		return ast.WalkSkipChildren, nil
	}
	if block.HasClosure() {
		if err := onebotBridgeWriteEscapedMarkdownBytes(writer, block.ClosureLine.Value(source)); err != nil {
			return ast.WalkContinue, err
		}
	}
	if _, err := writer.WriteString("</pre>"); err != nil {
		return ast.WalkContinue, err
	}
	return ast.WalkContinue, nil
}

func onebotBridgeRenderImagePlaceholder(writer util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	image := node.(*ast.Image)
	if _, err := writer.WriteString("[image: "); err != nil {
		return ast.WalkSkipChildren, err
	}
	if err := onebotBridgeWriteMarkdownImageAlt(writer, image, source); err != nil {
		return ast.WalkSkipChildren, err
	}
	if err := writer.WriteByte(']'); err != nil {
		return ast.WalkSkipChildren, err
	}
	return ast.WalkSkipChildren, nil
}

func onebotBridgeWriteMarkdownImageAlt(writer util.BufWriter, node ast.Node, source []byte) error {
	switch node := node.(type) {
	case *ast.CodeSpan, *ast.CodeBlock, *ast.FencedCodeBlock:
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			if err := onebotBridgeWriteMarkdownImageAltCode(writer, child, source); err != nil {
				return err
			}
		}
		return nil
	case *ast.Text:
		if node.IsRaw() {
			htmlrenderer.DefaultWriter.RawWrite(writer, node.Value(source))
		} else {
			htmlrenderer.DefaultWriter.Write(writer, node.Value(source))
		}
		if node.SoftLineBreak() || node.HardLineBreak() {
			return writer.WriteByte(' ')
		}
		return nil
	case *ast.String:
		return onebotBridgeWriteMarkdownImageAltString(writer, node, node.Value)
	case *ast.RawHTML:
		for index := range node.Segments.Len() {
			segment := node.Segments.At(index)
			if err := onebotBridgeWriteEscapedMarkdownBytes(writer, segment.Value(source)); err != nil {
				return err
			}
		}
		return nil
	case *ast.AutoLink:
		htmlrenderer.DefaultWriter.Write(writer, node.Label(source))
		return nil
	default:
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			if err := onebotBridgeWriteMarkdownImageAlt(writer, child, source); err != nil {
				return err
			}
		}
		return nil
	}
}

func onebotBridgeWriteMarkdownImageAltCode(writer util.BufWriter, node ast.Node, source []byte) error {
	switch node := node.(type) {
	case *ast.Text:
		htmlrenderer.DefaultWriter.RawWrite(writer, node.Value(source))
		return nil
	case *ast.String:
		htmlrenderer.DefaultWriter.RawWrite(writer, node.Value)
		return nil
	case *ast.RawHTML:
		for index := range node.Segments.Len() {
			segment := node.Segments.At(index)
			if err := onebotBridgeWriteEscapedMarkdownBytes(writer, segment.Value(source)); err != nil {
				return err
			}
		}
		return nil
	default:
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			if err := onebotBridgeWriteMarkdownImageAltCode(writer, child, source); err != nil {
				return err
			}
		}
		return nil
	}
}

func onebotBridgeWriteMarkdownImageAltString(writer util.BufWriter, node *ast.String, value []byte) error {
	if node.IsRaw() || node.IsCode() {
		htmlrenderer.DefaultWriter.RawWrite(writer, value)
	} else {
		htmlrenderer.DefaultWriter.Write(writer, value)
	}
	return nil
}

func onebotBridgeWriteEscapedMarkdownBytes(writer io.Writer, value []byte) error {
	start := 0
	for index, char := range value {
		var entity string
		switch char {
		case '&':
			entity = "&amp;"
		case '<':
			entity = "&lt;"
		case '>':
			entity = "&gt;"
		case '"':
			entity = "&#34;"
		case '\'':
			entity = "&#39;"
		default:
			continue
		}
		if err := onebotBridgeWriteAll(writer, value[start:index]); err != nil {
			return err
		}
		if _, err := io.WriteString(writer, entity); err != nil {
			return err
		}
		start = index + 1
	}
	return onebotBridgeWriteAll(writer, value[start:])
}

func onebotBridgeWriteAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
