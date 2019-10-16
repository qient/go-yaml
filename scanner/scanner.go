package scanner

import (
	"io"
	"strings"

	"github.com/goccy/go-yaml/token"
	"golang.org/x/xerrors"
)

type IndentState int

const (
	IndentStateEqual IndentState = iota
	IndentStateUp
	IndentStateDown
	IndentStateKeep
)

type Scanner struct {
	source            string
	sourcePos         int
	sourceSize        int
	line              int
	column            int
	offset            int
	prevIndentLevel   int
	prevIndentNum     int
	indentLevel       int
	indentNum         int
	isFirstCharAtLine bool
	isMapContext      bool
	isAnchor          bool
	indentState       IndentState
	savedPos          *token.Position
}

func (s *Scanner) pos() *token.Position {
	return &token.Position{
		Line:        s.line,
		Column:      s.column,
		Offset:      s.offset,
		IndentNum:   s.indentNum,
		IndentLevel: s.indentLevel,
	}
}

func (s *Scanner) bufferedToken(ctx *Context) *token.Token {
	if s.savedPos != nil {
		tk := ctx.bufferedToken(s.savedPos)
		s.savedPos = nil
		return tk
	}
	trimmedSrc := strings.TrimLeft(string(ctx.buf), " ")
	size := len(trimmedSrc)
	return ctx.bufferedToken(&token.Position{
		Line:        s.line,
		Column:      s.column - size,
		Offset:      s.offset - size,
		IndentNum:   s.indentNum,
		IndentLevel: s.indentLevel,
	})
}

func (s *Scanner) progressColumn(ctx *Context, num int) {
	s.column += num
	s.offset += num
	ctx.progress(num)
}

func (s *Scanner) progressLine(ctx *Context) {
	s.column = 1
	s.line++
	s.offset++
	s.indentNum = 0
	s.isFirstCharAtLine = true
	s.isAnchor = false
	ctx.progress(1)
}

func (s *Scanner) updateIndent(c rune) {
	if s.isFirstCharAtLine && c == ' ' {
		s.indentNum++
		return
	}
	if !s.isFirstCharAtLine {
		s.indentState = IndentStateKeep
		return
	}

	if s.prevIndentNum < s.indentNum {
		s.indentLevel = s.prevIndentLevel + 1
		s.indentState = IndentStateUp
	} else if s.prevIndentNum == s.indentNum {
		s.indentLevel = s.prevIndentLevel
		s.indentState = IndentStateEqual
	} else {
		s.indentState = IndentStateDown
		s.indentLevel = s.prevIndentLevel - 1
	}
	s.prevIndentNum = s.indentNum
	s.prevIndentLevel = s.indentLevel
	s.isFirstCharAtLine = false
}

func (s *Scanner) isChangedToIndentStateDown() bool {
	return s.indentState == IndentStateDown
}

func (s *Scanner) isChangedToIndentStateUp() bool {
	return s.indentState == IndentStateUp
}

func (s *Scanner) isChangedToIndentStateEqual() bool {
	return s.indentState == IndentStateEqual
}

func (s *Scanner) addBufferedTokenIfExists(ctx *Context) {
	ctx.addToken(s.bufferedToken(ctx))
}

func (s *Scanner) breakLiteral(ctx *Context) {
	s.isMapContext = false
	ctx.breakLiteral()
}

func (s *Scanner) scanQuote(ctx *Context, ch rune) (tk *token.Token, pos int) {
	ctx.addOriginBuf(ch)
	startIndex := ctx.idx + 1
	ctx.progress(1)
	for idx, c := range ctx.src[startIndex:] {
		pos = idx + 1
		ctx.addOriginBuf(c)
		switch c {
		case ch:
			if ctx.previousChar() == '\\' {
				continue
			}
			value := ctx.source(startIndex, startIndex+idx)
			switch ch {
			case '\'':
				tk = token.SingleQuote(value, string(ctx.obuf), s.pos())
			case '"':
				tk = token.DoubleQuote(value, string(ctx.obuf), s.pos())
			}
			pos = len(value) + 1
			return
		}
	}
	return
}

func (s *Scanner) scanTag(ctx *Context) (tk *token.Token, pos int) {
	ctx.addOriginBuf('!')
	ctx.progress(1) // skip '!' character
	for idx, c := range ctx.src[ctx.idx:] {
		pos = idx + 1
		ctx.addOriginBuf(c)
		switch c {
		case ' ', '\n':
			value := ctx.source(ctx.idx-1, ctx.idx+idx)
			tk = token.Tag(value, string(ctx.obuf), s.pos())
			pos = len(value)
			return
		}
	}
	return
}

func (s *Scanner) scanComment(ctx *Context) (tk *token.Token, pos int) {
	ctx.addOriginBuf('#')
	ctx.progress(1) // skip '#' character
	for idx, c := range ctx.src[ctx.idx:] {
		pos = idx + 1
		ctx.addOriginBuf(c)
		switch c {
		case '\n':
			if ctx.previousChar() == '\\' {
				continue
			}
			value := ctx.source(ctx.idx, ctx.idx+idx)
			tk = token.Comment(value, string(ctx.obuf), s.pos())
			pos = len(value) + 1
			return
		}
	}
	return
}

func (s *Scanner) scanLiteral(ctx *Context, c rune) {
	if ctx.isEOS() {
		value := ctx.bufferedSrc()
		ctx.addToken(token.New(value, string(ctx.obuf), s.pos()))
	}
	if c == '\n' {
		if ctx.isLiteral {
			ctx.addBuf(c)
		} else {
			ctx.addBuf(' ')
		}
		s.progressLine(ctx)
	} else if s.isFirstCharAtLine && c == ' ' {
		s.progressColumn(ctx, 1)
	} else {
		ctx.addBuf(c)
		s.progressColumn(ctx, 1)
	}
	ctx.addOriginBuf(c)
}

func (s *Scanner) scanLiteralHeader(ctx *Context) (pos int, err error) {
	header := ctx.currentChar()
	ctx.addOriginBuf(header)
	ctx.progress(1) // skip '|' or '<' character
	for idx, c := range ctx.src[ctx.idx:] {
		pos = idx
		ctx.addOriginBuf(c)
		switch c {
		case '\n':
			value := ctx.source(ctx.idx, ctx.idx+idx)
			opt := strings.TrimRight(value, " ")
			switch opt {
			case "", "+", "-",
				"0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
				if header == '|' {
					ctx.addToken(token.Literal("|"+opt, string(ctx.obuf), s.pos()))
					ctx.isLiteral = true
				} else if header == '>' {
					ctx.addToken(token.Folded(">"+opt, string(ctx.obuf), s.pos()))
					ctx.isFolded = true
				}
				ctx.resetBuffer()
				ctx.literalOpt = opt
				return
			}
			break
		}
	}
	err = xerrors.New("invalid literal header")
	return
}

func (s *Scanner) scanNewLine(ctx *Context, c rune) {
	if len(ctx.buf) > 0 && s.savedPos == nil {
		s.savedPos = s.pos()
	}
	if ctx.isEOS() {
		s.addBufferedTokenIfExists(ctx)
	}
	ctx.addBuf(' ')
	ctx.addOriginBuf(c)
	s.progressLine(ctx)
}

func (s *Scanner) scan(ctx *Context) (pos int) {
	for ctx.next() {
		pos = ctx.nextPos()
		c := ctx.currentChar()
		s.updateIndent(c)
		if s.isChangedToIndentStateDown() {
			s.addBufferedTokenIfExists(ctx)
			s.breakLiteral(ctx)
		} else if ctx.isLiteral || ctx.isFolded || ctx.isRawFolded {
			s.scanLiteral(ctx, c)
			continue
		} else if s.isMapContext && s.isChangedToIndentStateEqual() {
			s.addBufferedTokenIfExists(ctx)
			s.isMapContext = false
		} else if c != '-' && s.indentState == IndentStateUp {
			s.isMapContext = false
		}
		switch c {
		case '{':
			ctx.addOriginBuf(c)
			ctx.addToken(token.MappingStart(string(ctx.obuf), s.pos()))
			s.progressColumn(ctx, 1)
			return
		case '}':
			ctx.addToken(s.bufferedToken(ctx))
			ctx.addOriginBuf(c)
			ctx.addToken(token.MappingEnd(string(ctx.obuf), s.pos()))
			s.progressColumn(ctx, 1)
			return
		case '.':
			if s.indentNum == 0 && ctx.repeatNum('.') == 3 {
				ctx.addToken(token.DocumentEnd(s.pos()))
				s.progressColumn(ctx, 3)
				pos += 2
				return
			}
		case '<':
			if ctx.repeatNum('<') == 2 {
				ctx.addToken(s.bufferedToken(ctx))
				ctx.addToken(token.MergeKey(s.pos()))
				s.progressColumn(ctx, 1)
				pos++
				return
			}
		case '-':
			if s.indentNum == 0 && ctx.repeatNum('-') == 3 {
				s.addBufferedTokenIfExists(ctx)
				ctx.addToken(token.DocumentHeader(s.pos()))
				s.progressColumn(ctx, 3)
				pos += 2
				return
			}
			if !s.isMapContext && s.isChangedToIndentStateUp() {
				// raw folded
				ctx.isRawFolded = true
				ctx.addBuf(c)
				ctx.addOriginBuf(c)
				s.progressColumn(ctx, 1)
				continue
			}
			nc := ctx.nextChar()
			if nc == ' ' {
				s.addBufferedTokenIfExists(ctx)
				ctx.addOriginBuf(c)
				ctx.addToken(token.SequenceEntry(string(ctx.obuf), s.pos()))
				s.progressColumn(ctx, 1)
				return
			}
		case '[':
			ctx.addOriginBuf(c)
			ctx.addToken(token.SequenceStart(string(ctx.obuf), s.pos()))
			s.progressColumn(ctx, 1)
			return
		case ']':
			s.addBufferedTokenIfExists(ctx)
			ctx.addOriginBuf(c)
			ctx.addToken(token.SequenceEnd(string(ctx.obuf), s.pos()))
			s.progressColumn(ctx, 1)
			return
		case ',':
			s.addBufferedTokenIfExists(ctx)
			ctx.addOriginBuf(c)
			ctx.addToken(token.CollectEntry(string(ctx.obuf), s.pos()))
			s.progressColumn(ctx, 1)
			return
		case ':':
			nc := ctx.nextChar()
			if nc == ' ' || nc == '\n' {
				// mapping value
				s.isMapContext = true
				s.addBufferedTokenIfExists(ctx)
				ctx.addToken(token.MappingValue(s.pos()))
				s.progressColumn(ctx, 1)
				return
			}
		case '|', '>':
			if ctx.bufferedSrc() == "" {
				progress, err := s.scanLiteralHeader(ctx)
				if err != nil {
					// TODO: returns syntax error object
					return
				}
				s.progressColumn(ctx, progress)
				s.progressLine(ctx)
				continue
			}
		case '!':
			token, progress := s.scanTag(ctx)
			ctx.addToken(token)
			s.progressColumn(ctx, progress)
			if c := ctx.previousChar(); c == '\n' {
				s.progressLine(ctx)
			}
			pos += progress
			return
		case '%':
			if s.indentNum == 0 {
				ctx.addToken(token.Directive(s.pos()))
				s.progressColumn(ctx, 1)
				return
			} else {
				// TODO: returns syntax error object
			}
		case '?':
			nc := ctx.nextChar()
			if nc == ' ' {
				ctx.addToken(token.Directive(s.pos()))
				s.progressColumn(ctx, 1)
				return
			}
		case '&':
			s.addBufferedTokenIfExists(ctx)
			ctx.addOriginBuf(c)
			ctx.addToken(token.Anchor(string(ctx.obuf), s.pos()))
			s.progressColumn(ctx, 1)
			s.isAnchor = true
			return
		case '*':
			s.addBufferedTokenIfExists(ctx)
			ctx.addOriginBuf(c)
			ctx.addToken(token.Alias(string(ctx.obuf), s.pos()))
			s.progressColumn(ctx, 1)
			return
		case '#':
			s.addBufferedTokenIfExists(ctx)
			token, progress := s.scanComment(ctx)
			ctx.addToken(token)
			s.progressColumn(ctx, progress)
			s.progressLine(ctx)
			pos += progress
			return
		case '\'', '"':
			token, progress := s.scanQuote(ctx, c)
			ctx.addToken(token)
			s.progressColumn(ctx, progress)
			pos += progress
			return
		case '\n':
			s.scanNewLine(ctx, c)
			continue
		case ' ':
			if ctx.isSaveIndentMode() || (!s.isAnchor && !s.isFirstCharAtLine) {
				ctx.addBuf(c)
				ctx.addOriginBuf(c)
				s.progressColumn(ctx, 1)
				continue
			}
			if s.isFirstCharAtLine {
				s.progressColumn(ctx, 1)
				ctx.addOriginBuf(c)
				continue
			}
			s.addBufferedTokenIfExists(ctx)
			s.progressColumn(ctx, 1)
			s.isAnchor = false
			return
		}
		ctx.addBuf(c)
		ctx.addOriginBuf(c)
		s.progressColumn(ctx, 1)
	}
	return
}

func (s *Scanner) Init(src string) {
	s.source = src
	s.sourcePos = 0
	s.sourceSize = len(src)
	s.line = 1
	s.column = 1
	s.offset = 1
	s.prevIndentLevel = 0
	s.prevIndentNum = 0
	s.indentLevel = 0
	s.indentNum = 0
	s.isFirstCharAtLine = true
}

func (s *Scanner) Scan() (token.Tokens, error) {
	if s.sourcePos >= s.sourceSize {
		return nil, io.EOF
	}
	ctx := newContext(s.source[s.sourcePos:])
	progress := s.scan(ctx)
	s.sourcePos += progress
	return ctx.tokens, nil
}