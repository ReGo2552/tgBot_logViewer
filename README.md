# logviewer

Telegram-бот, который по запросу присылает последние строки логов с сервера:
systemd-служб (journald), docker-контейнеров и текстовых файлов. Без SSH.

```
/logs                  → кнопки: ⚙️ Службы · 🐳 Контейнеры · 📄 Файлы
/logs caddy            → последние 100 строк caddy
/logs mc_bot 500 error → строки с «error» среди последних 5000
```

Под каждым ответом кнопки: другое число строк, 🔄 обновить, 📎 прислать файлом.
Длинный вывод бот сам отправляет файлом `.txt`.

Один статический бинарник для Linux, без зависимостей на сервере.
Поддерживаются Debian и Ubuntu с systemd; docker или podman по желанию.

## Как устроено

- **Показывается только то, что перечислено в конфиге.** В чате пользователь выбирает
  источник из списка и никогда не передаёт путь или имя контейнера. Прочитать через
  бота `/etc/shadow` нельзя.
- **Конфиг не надо писать вручную.** `logviewer discover` находит службы, контейнеры
  (по compose-стекам) и лог-файлы программ и пишет черновик. Очевидное включено, всё
  остальное закомментировано: что показывать, решаете вы.
- **Бот работает от отдельного пользователя `logbot`** без shell и домашнего каталога:
  - journald он читает через группу `systemd-journal`;
  - группу `adm` не получает, чтобы не видеть `auth.log` и `syslog`;
  - файлы читает по обычным правам, узкий доступ выдаётся через `setfacl`;
  - в docker ходит через правило sudo на одну команду (об этом ниже).
- **Секреты в выводе маскируются**: токены ботов, `user:pass@` в URL,
  `password=…`, `token: …`, `Authorization`, JWT, AWS-ключи и свои шаблоны.
  Это страховка, а не гарантия: логи с особо чувствительными данными лучше не
  добавлять в конфиг.
- **Отвечает только админам из конфига и только в личных сообщениях.** Попытки
  чужих пользователей пишутся в журнал бота.
- **Все запросы записываются** в журнал: кто, что и сколько строк.

### Docker без root

Членство в группе `docker` равносильно root, поэтому `logbot` в неё не добавляется.
Вместо этого в `/etc/sudoers.d/logviewer` ему разрешена ровно одна команда:

```
logbot ALL=(root) NOPASSWD: /usr/local/bin/logviewer docker-helper *
```

`docker-helper` работает от root и сам проверяет запрос:
- конфиг читается только из `/etc/log-viewer/config.yaml`, и только если
  его не может изменить никто, кроме root;
- имя контейнера должно подходить под источник из конфига;
- умеет только `docker ps` и `docker logs --tail N`.

Если вы сознательно готовы дать боту группу `docker`, поставьте `docker.access: direct`.
Тогда sudo не нужен, а юнит получает более строгую изоляцию.

## Установка

Нужен Go 1.24+ на машине, где собираете (на сервере не нужен).

```bash
make dist        # dist/logviewer-linux-amd64 и -arm64
scp dist/logviewer-linux-amd64 server:/tmp/logviewer
```

На сервере:

```bash
sudo /tmp/logviewer install --timezone Europe/Moscow
```

Установщик по шагам:
1. создаёт пользователя `logbot` и добавляет его в `systemd-journal`;
2. копирует себя в `/usr/local/bin/logviewer`;
3. спрашивает ваш Telegram ID (узнать можно у [@userinfobot](https://t.me/userinfobot))
   и пишет черновик конфига `/etc/log-viewer/config.yaml` через `discover`;
4. создаёт шаблон секретов `/etc/log-viewer/.env` (права `600 root:root`);
5. ставит sudo-правило для docker (проверяется через `visudo`) и юнит `logviewer.service`;
6. запускает `doctor` от имени `logbot` и печатает, что ещё поправить.

Дальше:

```bash
sudo nano /etc/log-viewer/.env          # BOT_TOKEN от @BotFather, PROXY_URL при необходимости
sudo nano /etc/log-viewer/config.yaml   # просмотреть источники
sudo logviewer doctor                   # всё ✅? тогда
sudo systemctl start logviewer
```

Повторный `install` обновляет бинарник, юнит и sudo-правило, а конфиг и `.env` не трогает.
Так же обновляется версия: соберите новый бинарник и запустите `install` ещё раз.

### Прокси

Если `api.telegram.org` недоступен напрямую, укажите в `.env`:

```
PROXY_URL=socks5h://user:password@host:port
```

Поддерживаются `http`, `https`, `socks5`, `socks5h`. У `socks5h` DNS-запросы идут
через прокси. Переменные `HTTPS_PROXY` из окружения бот не использует, прокси задаётся
только явно.

## Конфиг

Полный пример с комментариями: [`config.example.yaml`](config.example.yaml).

```yaml
admins: [123456789]
timezone: Europe/Moscow
sources:
  - {id: caddy,     type: systemd, unit: caddy.service}
  - {id: immich,    type: docker,  match: "stack:immich"}   # все контейнеры compose-проекта
  - {id: kuma,      type: docker,  container: uptime-kuma}
  - {id: minecraft, type: file,    path: /srv/minecraft/logs/latest.log}
  - {id: nginx,     type: file,    path: "/var/log/nginx/*.log"}
```

| Поле | Что это |
|---|---|
| `id` | имя в боте и в `/logs id` |
| `title` | подпись кнопки, если хочется красивее, чем `id` |
| `unit` | systemd-юнит; маски не поддерживаются |
| `container` | один контейнер |
| `match` | `stack:проект` (метка compose-проекта) или маска по имени: `web_*`, `*` |
| `path` | файл или маска; при нескольких файлах бот предложит выбрать |

После правки: `sudo logviewer doctor && sudo systemctl restart logviewer`.

Команда `/discover` в боте покажет, что появилось на сервере, но ещё не добавлено в конфиг.
Ответ придёт готовыми строками для вставки. Сам конфиг бот не меняет: у него нет прав на запись.

## Команды

| Где | Команда | Что делает |
|---|---|---|
| бот | `/logs` | меню источников |
| бот | `/logs id [N] [фильтр]` | последние N строк; с фильтром ищет среди последних `filter_window` |
| бот | `имя_контейнера` | контейнер из стека можно запросить по имени |
| бот | `/discover`, `/doctor` | новые кандидаты; проверка доступа |
| сервер | `sudo logviewer discover` | черновик конфига (`--out FILE`, `--admin`, `--timezone`) |
| сервер | `sudo logviewer doctor` | конфиг, токен, прокси, доступ `logbot` к каждому источнику |
| сервер | `logviewer show id [N] [фильтр]` | то же, что покажет бот, но в консоль |
| сервер | `journalctl -u logviewer -f` | журнал самого бота, включая аудит запросов |

## Логи Python-ботов

Отдельный тип источника не нужен, логи зависят от способа запуска:

- **systemd-юнит**: вывод идёт в journald, источник `type: systemd`;
- **docker**: источник `type: docker`;
- **`nohup`/`screen` с записью в файл**: источник `type: file`.

Python буферизует stdout, если пишет не в терминал. `print()` доходит до
journald пачками, а при падении последние строки теряются. Лечится так:

```ini
[Service]
Environment=PYTHONUNBUFFERED=1
```

(или `python -u`). Стектрейсы идут в stderr и не буферизуются.

## Удаление

```bash
sudo systemctl disable --now logviewer
sudo rm /etc/systemd/system/logviewer.service /etc/sudoers.d/logviewer /usr/local/bin/logviewer
sudo rm -r /etc/log-viewer
sudo userdel logbot
sudo systemctl daemon-reload
```

Выданные через `setfacl` права удаляются так: `sudo setfacl -x u:logbot ПУТЬ`.

## Разработка

```bash
make test     # go vet + тесты
make build    # ./logviewer
```

## Лицензия

MIT, см. [LICENSE](LICENSE).
