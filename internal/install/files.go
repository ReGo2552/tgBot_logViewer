package install

import (
	"fmt"
	"strings"

	"github.com/ReGo2552/tgBot_logViewer/internal/config"
)

const (
	UnitPath    = "/etc/systemd/system/logviewer.service"
	SudoersPath = "/etc/sudoers.d/logviewer"
)

// Unit — systemd-юнит. Для sudo (docker.access: sudo) нельзя NoNewPrivileges и всё,
// что его подразумевает (PrivateDevices, ProtectKernel*, RestrictSUIDSGID,
// SystemCallFilter и др.): иначе sudo не сможет поднять права даже на одну команду.
func Unit(user string, needSudo bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[Unit]
Description=logviewer: логи сервера в Telegram
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=%[1]s
Group=%[1]s
EnvironmentFile=%[2]s
ExecStart=%[3]s run --config %[4]s
Restart=always
RestartSec=10

# Изоляция. /home только на чтение, а не скрыт: логи бывают и там;
# доступ всё равно решают обычные права файлов.
ProtectSystem=full
ProtectHome=read-only
PrivateTmp=true
ProtectControlGroups=true
`, user, config.EnvPath, config.BinaryPath, config.DefaultPath)
	if needSudo {
		b.WriteString(`# NoNewPrivileges выключен ради одной команды через sudo:
# logviewer docker-helper (правило в /etc/sudoers.d/logviewer).
NoNewPrivileges=false
`)
	} else {
		b.WriteString(`NoNewPrivileges=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectClock=true
RestrictSUIDSGID=true
RestrictNamespaces=true
LockPersonality=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
`)
	}
	b.WriteString(`
[Install]
WantedBy=multi-user.target
`)
	return b.String()
}

// Sudoers — одна команда. Аргументы проверяет сам docker-helper: он запускается от root,
// читает конфиг только из /etc/log-viewer (владелец root) и отдаёт логи лишь тех
// контейнеров, что перечислены в конфиге.
func Sudoers(user string) string {
	return fmt.Sprintf(`# logviewer: пользователь %[1]s читает логи docker-контейнеров из конфига — и только.
# Создано командой: logviewer install
%[1]s ALL=(root) NOPASSWD: %[2]s docker-helper *

# Без PAM-сессии: иначе каждый запрос логов добавляет в журнал две строки
# «session opened/closed». Строка с самой командой (аудит) остаётся.
Defaults:%[1]s !pam_session
`, user, config.BinaryPath)
}

const envTemplate = `# Секреты logviewer. Права 600 root:root: systemd читает файл до сброса прав.
# После правки: sudo systemctl restart logviewer

# Токен бота от @BotFather.
BOT_TOKEN=

# Прокси до api.telegram.org, если Telegram заблокирован. Пусто — напрямую.
# socks5h:// (с h) — DNS резолвится на стороне прокси, так надёжнее.
# PROXY_URL=socks5h://user:password@host:port
PROXY_URL=

# Свой сервер Bot API (обычно не нужно).
# API_URL=https://api.telegram.org
`
