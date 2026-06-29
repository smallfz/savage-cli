package main

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

type textAccessor struct {
	content string
}

func (t *textAccessor) Get() string {
	return t.content
}

func (t *textAccessor) Set(v string) {
	t.content = v
}

type syncMsg struct{}

type tuiModelV4 struct {
	hist       *hist
	histCur    int
	form       *huh.Form
	text       *huh.Text
	ta         *textAccessor
	quit       bool
	hotContent string
}

var _ tea.Model = (*tuiModelV4)(nil)

func newTUIModelV4(h *hist, form *huh.Form, text *huh.Text, ta *textAccessor) *tuiModelV4 {
	return &tuiModelV4{
		hist: h,
		form: form,
		text: text,
		ta:   ta,
	}
}

func (t *tuiModelV4) Init() tea.Cmd {
	return t.form.Init()
}

func (t *tuiModelV4) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyPressMsg:
		switch m.String() {
		case "ctrl+q":
			t.quit = true
			return t, tea.Quit
		case "up":
			if t.histCur == 0 {
				content := t.ta.Get()
				t.hotContent = content
			}
			t.histCur += 1
			t.ta.Set(t.hist.At(t.histCur - 1))
			t.text.Accessor(t.ta)
			// return t, nil
		case "down":
			t.histCur -= 1
			if t.histCur <= 0 {
				t.histCur = 0
			}
			if t.histCur <= 0 {
				t.ta.Set(t.hotContent)
			} else {
				content := t.hist.At(t.histCur - 1)
				t.ta.Set(content)
			}
			t.text.Accessor(t.ta)
			// return t, nil
		default:
			t.histCur = 0
			t.hotContent = ""
		}
	}

	formModel, cmd := t.form.Update(msg)
	if f, ok := formModel.(*huh.Form); ok {
		t.form = f
	}

	switch t.form.State {
	case huh.StateCompleted, huh.StateAborted:
		return t, tea.Quit
	}

	return t, cmd
}

func (t *tuiModelV4) View() tea.View {
	return tea.NewView(t.form.View())
}
