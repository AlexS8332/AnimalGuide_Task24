package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
)

// Блокнот в памяти: initialize называет animals-notes, tools/list — три
// инструмента блокнота и server_info, цикл open → add → close пишет файл в
// <data>/notes.
func TestNotesServer(t *testing.T) {
	data := t.TempDir()
	srv := notesServer(data, slog.New(slog.DiscardHandler))
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.SDK().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	init := cs.InitializeResult()
	if init.ServerInfo.Name != notes.ServerName || init.ServerInfo.Title != "Блокнот натуралиста" || init.Instructions == "" {
		t.Errorf("initialize: %+v, инструкция %q", init.ServerInfo, init.Instructions)
	}
	var names []string
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
		if tool.Name != mcp.InfoTool && (tool.Annotations == nil || tool.Annotations.ReadOnlyHint) {
			t.Errorf("%s помечен «только чтение»", tool.Name)
		}
	}
	if want := append(append([]string(nil), notes.ToolNames...), mcp.InfoTool); !sameSet(names, want) {
		t.Errorf("инструменты %v, ждали %v", names, want)
	}

	call := func(name string, args any, out any) {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		if res.IsError {
			t.Fatalf("%s: %s", name, text)
		}
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var op notes.OpenResult
	call(notes.ToolOpen, notes.OpenArgs{Title: "Манул", Species: "Otocolobus manul"}, &op)
	call(notes.ToolAdd, notes.AddArgs{NotebookID: op.NotebookID, Heading: "Питание", Text: "Пищухи.",
		Cites: []string{"read_wikipedia"}}, &notes.AddResult{})
	var cl notes.CloseResult
	call(notes.ToolClose, notes.CloseArgs{NotebookID: op.NotebookID}, &cl)
	if filepath.Dir(cl.Path) != filepath.Join(data, "notes") {
		t.Errorf("файл %q не в <data>/notes", cl.Path)
	}
	if _, err := os.Stat(cl.Path); err != nil {
		t.Error(err)
	}

	// Выдуманный id — ошибка инструмента (IsError) словами, а не сбой протокола.
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: notes.ToolAdd,
		Arguments: notes.AddArgs{NotebookID: "nb-123", Heading: "x", Text: "y"}})
	if err != nil || !res.IsError {
		t.Errorf("выдуманный id: err=%v res=%+v", err, res)
	}

	var info mcp.Info
	call(mcp.InfoTool, nil, &info)
	if info.Server != notes.ServerName || info.Calls[notes.ToolOpen] != 1 || info.Calls[notes.ToolAdd] != 2 {
		t.Errorf("server_info: %+v", info)
	}
}

func sameSet(a, b []string) bool {
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}
