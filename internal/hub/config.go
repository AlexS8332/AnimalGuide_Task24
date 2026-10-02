package hub

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
)

// defaultJSON — встроенная конфигурация. Файлом, а не литералом Go: её же
// копия лежит в корне репозитория (mcp-servers.example.json) как образец
// для своей, и тест следит, чтобы они не разошлись.
//
//go:embed default.json
var defaultJSON []byte

// Подстановки в Command, Args и URL.
const (
	varData   = "${DATA}"
	varDaemon = "${DAEMON}"
)

// DefaultConfig — встроенная конфигурация (default.json): sources, daemon,
// notes. Подстановки ${DATA} и ${DAEMON} в ней не раскрыты — это делает
// LoadConfig.
func DefaultConfig() Config {
	cfg, err := parseConfig(defaultJSON)
	if err != nil {
		// Встроенный файл проверяет тест: сломанный — ошибка сборки.
		panic("hub: встроенная конфигурация: " + err.Error())
	}
	return cfg
}

// LoadConfig читает конфигурацию из файла; пустой путь — встроенная.
// Подставляет ${DATA} и ${DAEMON} и проверяет её (Validate).
//
// Пустой DataDir — рабочий каталог («.»). Пустой DaemonURL — адрес демона
// по умолчанию (feed.DefaultServer): демон — отдельный процесс, и если он
// не запущен, сервер получит статус down с подсказкой, как его поднять, а
// не молча пропадёт из реестра.
func LoadConfig(path string, d Defaults) (Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("%w: %v", ErrConfig, err)
		}
		if cfg, err = parseConfig(data); err != nil {
			return Config{}, fmt.Errorf("%w: %s: %v", ErrConfig, path, err)
		}
	}
	if d.DataDir == "" {
		d.DataDir = "."
	}
	if d.DaemonURL == "" {
		d.DaemonURL = feed.DefaultServer
	}
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		s.Command = expand(s.Command, d)
		s.URL = expand(s.URL, d)
		args := make([]string, len(s.Args))
		for j, a := range s.Args {
			args[j] = expand(a, d)
		}
		s.Args = args
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseConfig — JSON строго: опечатка в имени поля («tool» вместо
// «tools») иначе тихо дала бы сервер без инструментов.
func parseConfig(data []byte) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func expand(s string, d Defaults) string {
	s = strings.ReplaceAll(s, varData, d.DataDir)
	return strings.ReplaceAll(s, varDaemon, d.DaemonURL)
}

// Validate — имена серверов уникальны и непусты, транспорт известен, у
// stdio нет URL, у http есть URL, одно имя инструмента не выдано двум
// серверам. Ошибка оборачивает ErrConfig.
//
// Пересечение списков проверяется с шаблонами: «mdd_*» у одного сервера и
// «mdd_get» у другого — такой же конфликт, как два «mdd_get». Иначе
// маршрут зависел бы от порядка серверов в файле, а это ровно то, чего
// реестр обещает не делать.
func (c Config) Validate() error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("%w: нет ни одного сервера", ErrConfig)
	}
	seen := map[string]bool{}
	for i, s := range c.Servers {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return fmt.Errorf("%w: у сервера №%d нет имени", ErrConfig, i+1)
		}
		if name != s.Name || strings.ContainsAny(name, " \t/\\") {
			return fmt.Errorf("%w: имя сервера %q — без пробелов и косых черт", ErrConfig, s.Name)
		}
		if seen[name] {
			return fmt.Errorf("%w: сервер %s объявлен дважды", ErrConfig, name)
		}
		seen[name] = true
		switch s.Transport {
		case TransportStdio:
			if s.URL != "" {
				return fmt.Errorf("%w: у stdio-сервера %s не бывает url — это процесс, его запускают командой", ErrConfig, name)
			}
		case TransportHTTP:
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("%w: у http-сервера %s нет url", ErrConfig, name)
			}
			if s.Command != "" || len(s.Args) > 0 {
				return fmt.Errorf("%w: у http-сервера %s не бывает command и args — его запускают отдельно", ErrConfig, name)
			}
		default:
			return fmt.Errorf("%w: у сервера %s транспорт %q — нужен stdio или http", ErrConfig, name, s.Transport)
		}
		if strings.Contains(s.URL, "${") || strings.Contains(s.Command, "${") {
			return fmt.Errorf("%w: у сервера %s неизвестная подстановка: есть только %s и %s", ErrConfig, name, varData, varDaemon)
		}
		for _, a := range s.Args {
			if strings.Contains(a, "${") {
				return fmt.Errorf("%w: у сервера %s в args неизвестная подстановка %q: есть только %s и %s", ErrConfig, name, a, varData, varDaemon)
			}
		}
		for _, p := range s.Tools {
			switch {
			case strings.TrimSpace(p) == "" || p != strings.TrimSpace(p):
				return fmt.Errorf("%w: у сервера %s в tools пустое имя или пробелы по краям: %q", ErrConfig, name, p)
			case strings.Contains(strings.TrimSuffix(p, "*"), "*"):
				return fmt.Errorf("%w: у сервера %s шаблон %q: «*» допускается только в конце", ErrConfig, name, p)
			case p == mcp.InfoTool:
				return fmt.Errorf("%w: у сервера %s в tools %s — служебный инструмент модели не выдаётся", ErrConfig, name, mcp.InfoTool)
			}
		}
	}
	for i, a := range c.Servers {
		for _, b := range c.Servers[i+1:] {
			for _, pa := range a.Tools {
				for _, pb := range b.Tools {
					if overlap(pa, pb) {
						return fmt.Errorf("%w: инструмент %s выдан двум серверам — %s (%s) и %s (%s); оставь его в списке одного",
							ErrConfig, example(pa, pb), a.Name, pa, b.Name, pb)
					}
				}
			}
		}
	}
	return nil
}

// allowed — выдан ли инструмент name списком allow (имена и шаблоны с «*»
// в конце).
func allowed(allow []string, name string) bool {
	for _, p := range allow {
		if match(p, name) {
			return true
		}
	}
	return false
}

func match(pattern, name string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return pattern == name
}

// overlap — есть ли имя, которое подходит под оба шаблона. Два шаблона
// пересекаются, если префикс одного — начало другого; имя и шаблон — если
// имя подходит под шаблон.
func overlap(a, b string) bool {
	pa, wa := strings.CutSuffix(a, "*")
	pb, wb := strings.CutSuffix(b, "*")
	switch {
	case wa && wb:
		return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
	case wa:
		return strings.HasPrefix(b, pa)
	case wb:
		return strings.HasPrefix(a, pb)
	}
	return a == b
}

// example — имя из пересечения для текста ошибки: точное имя, если оно
// есть, иначе более длинный шаблон.
func example(a, b string) string {
	if !strings.HasSuffix(a, "*") {
		return a
	}
	if !strings.HasSuffix(b, "*") || len(b) > len(a) {
		return b
	}
	return a
}
