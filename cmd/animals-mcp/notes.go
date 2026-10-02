package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
)

// notesInstructions — инструкция блокнота в ответе initialize: порядок
// работы и откуда брать текст разделов.
const notesInstructions = "Блокнот натуралиста: записи о виде, которые становятся файлом Markdown или JSON. " +
	"Порядок: nb_open (title, species) → notebook_id; nb_add по разделу за вызов с этим notebook_id — " +
	"заголовок, текст и cites: имена инструментов, из ответов которых взяты сведения; nb_close — файл " +
	"записан, перечитан и сверен по sha256. notebook_id случайный: бери его только из ответа nb_open. " +
	"Сети у сервера нет, сведения о виде он не ищет — их приносят другие инструменты."

// notesServer — сервер блокнота над каталогом <data>/notes.
func notesServer(dataDir string, log *slog.Logger) *mcp.Server {
	return mcp.NewServer(notes.Tools(filepath.Join(dataDir, "notes")), mcp.ServerOptions{
		Name: notes.ServerName, Title: "Блокнот натуралиста", Instructions: notesInstructions, Logger: log,
	})
}

// runNotes — stdio-сервер «блокнот натуралиста» (-role notes): инструменты
// internal/notes над каталогом <data>/notes. Сеть не нужна, модель тоже.
// Возвращает код выхода.
func runNotes(dataDir string, log *slog.Logger) int {
	// Ctrl+C закрывает соединение так же, как закрытый клиентом stdin.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv := notesServer(dataDir, log)
	log.Info("блокнот запущен", "transport", "stdio", "version", mcp.Version, "dir", filepath.Join(dataDir, "notes"))
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "блокнот остановлен с ошибкой:", err)
		return 1
	}
	return 0
}
