// package edit implements a full-feature line editor.
package edit

import (
	"os"
	"fmt"
	"./tty"
	"../eval"
	"../parse"
	"../util"
)

var Lackeol = "\033[7m\u23ce\033[m\n"

type bufferMode int

const (
	ModeInsert bufferMode = iota
	ModeCommand
	ModeCompleting
)

type editorState struct {
	// States used during ReadLine.
	tokens []parse.Item
	prompt, rprompt, line string
	dot int
	tips []string
	mode bufferMode
	completion *completion
}

func (bs *editorState) finish() {
	// Clean up the state before exiting the editor.
	bs.tips = nil
	bs.mode = ModeInsert
	bs.completion = nil
	bs.dot = len(bs.line)
	// TODO Perhaps make it optional to NOT clear the rprompt
	bs.rprompt = ""
}

// Editor keeps the status of the line editor.
type Editor struct {
	savedTermios *tty.Termios
	file *os.File
	writer *writer
	reader *reader
	ev *eval.Evaluator
	editorState
}

// LineRead is the result of ReadLine. Exactly one member is non-zero, making
// it effectively a tagged union.
type LineRead struct {
	Line string
	Eof bool
	Err error
}

// Init initializes an Editor on the terminal referenced by fd.
// The Editor is reinitialized every time the control of the terminal is
// transferred back to the line editor.
func Init(file *os.File, tr *util.TimedReader, ev *eval.Evaluator) (*Editor, error) {
	fd := int(file.Fd())
	term, err := tty.NewTermiosFromFd(fd)
	if err != nil {
		return nil, fmt.Errorf("Can't get terminal attribute: %s", err)
	}

	editor := &Editor{
		savedTermios: term.Copy(),
		file: file,
		writer: newWriter(file),
		reader: newReader(tr),
		ev: ev,
	}

	term.SetIcanon(false)
	term.SetEcho(false)
	term.SetMin(1)
	term.SetTime(0)

	err = term.ApplyToFd(fd)
	if err != nil {
		return nil, fmt.Errorf("Can't set up terminal attribute: %s", err)
	}

	fmt.Fprint(editor.file, "\033[?7l")

	err = tty.FlushInput(fd)
	if err != nil {
		return nil, err
	}

	file.WriteString("\033[6n")
	// XXX Possible race condition: user input sneaked in between WriteString
	// and readCPR
	x, _, err := editor.reader.readCPR()
	if err != nil {
		return nil, err
	}

	if x != 1 {
		file.WriteString(Lackeol)
	}

	return editor, nil
}

// Cleanup restores the terminal referenced by fd so that other commands
// that use the terminal can be executed.
func (ed *Editor) Cleanup() error {
	fmt.Fprint(ed.file, "\033[?7h")

	fd := int(ed.file.Fd())
	err := ed.savedTermios.ApplyToFd(fd)
	if err != nil {
		return fmt.Errorf("Can't restore terminal attribute of stdin: %s", err)
	}
	ed.savedTermios = nil
	return nil
}

func (ed *Editor) beep() {
}

func (ed *Editor) pushTip(more string) {
	ed.tips = append(ed.tips, more)
}

func (ed *Editor) refresh() error {
	// Re-lex the line, unless we are in ModeCompleting
	if ed.mode != ModeCompleting {
		ed.tokens = nil
		hl := Highlight("<interactive code>", ed.line, ed.ev)
		for token := range hl {
			ed.tokens = append(ed.tokens, token)
		}
	}
	return ed.writer.refresh(&ed.editorState)
}

// TODO Allow modifiable keybindings.
var keyBindings = map[bufferMode]map[Key]string {
	ModeCommand: map[Key]string{
		Key{'i', 0}: "insert-mode",
		Key{'h', 0}: "move-dot-b",
		Key{'l', 0}: "move-dot-f",
		Key{'D', 0}: "kill-line-f",
		DefaultBinding: "default-command",
	},
	ModeInsert: map[Key]string{
		Key{'[', Ctrl}: "command-mode",
		Key{'U', Ctrl}: "kill-line-b",
		Key{'K', Ctrl}: "kill-line-f",
		Key{Backspace, 0}: "kill-rune-b",
		Key{Left, 0}: "move-dot-b",
		Key{Right, 0}: "move-dot-f",
		Key{Enter, 0}: "accept-line",
		Key{Tab, 0}: "complete",
		Key{'D', Ctrl}: "return-eof",
		DefaultBinding: "default-insert",
	},
	ModeCompleting: map[Key]string{
		Key{'[', Ctrl}: "cancel-completion",
		Key{Up, 0}: "select-cand-b",
		Key{Down, 0}: "select-cand-f",
		Key{Tab, 0}: "cycle-cand-f",
		DefaultBinding: "default-completing",
	},
}

// Accpet currently selected completion candidate.
func (ed *Editor) acceptCompletion() {
	c := ed.completion
	if 0 <= c.current && c.current < len(c.candidates) {
		accepted := c.candidates[c.current].text
		ed.line = ed.line[:c.start] + accepted + ed.line[c.end:]
		ed.dot += len(accepted) - (c.end - c.start)
	}
	ed.completion = nil
	ed.mode = ModeInsert
}

// ReadLine reads a line interactively.
func (ed *Editor) ReadLine(prompt string, rprompt string) (lr LineRead) {
	ed.prompt = prompt
	ed.rprompt = rprompt
	ed.line = ""
	ed.mode = ModeInsert
	ed.tips = nil
	ed.completion = nil
	ed.dot = 0

	for {
		err := ed.refresh()
		if err != nil {
			return LineRead{Err: err}
		}

		ed.tips = nil

		k, err := ed.reader.readKey()
		if err != nil {
			ed.pushTip(err.Error())
			continue
		}

		lookup_key:
		keyBinding, ok := keyBindings[ed.mode]
		if !ok {
			ed.pushTip("No binding for current mode")
			continue
		}

		name, bound := keyBinding[k]
		if !bound {
			name = keyBinding[DefaultBinding]
		}
		ret := leBuiltins[name](ed, k)
		if ret == nil {
			continue
		}
		switch ret.action {
		case noAction:
			continue
		case changeMode:
			ed.mode = ret.newMode
			continue
		case changeModeAndReprocess:
			ed.mode = ret.newMode
			goto lookup_key
		case exitReadLine:
			ed.finish()
			ed.refresh() // XXX Ignore possible error
			fmt.Fprintln(ed.file)
			return ret.readLineReturn
		}
	}
}
