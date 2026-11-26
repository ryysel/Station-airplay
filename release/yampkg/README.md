# Сборка модельных `.yampkg`

Публичное дерево собирает universal legacy `.yap`. Пакеты YAM v2
(`.yampkg`) добавляют к тому же runtime манифест, Luna-launcher и начальный
`airplay-controls-v2.json`. Этот каталог хранит матрицу целей и канонический
launcher, а `scripts/package_yampkg.py` пересобирает пакеты из опубликованного
базового релиза.

## Что лежит здесь

- `targets.json` — поведение каждой цели: передаётся ли `--controls-config`,
  сколько `sleep`, нужен ли `DBUS_SYSTEM_BUS_ADDRESS`, включён ли guard для
  `mango`, какие model ID добавить в `plugin.toml`, какие файлы взять из
  runtime и начальные флаги JSON.
- `launch-airplay.sh.in` — общий launcher. Плейсхолдеры подставляет сборщик.
- `requirements.txt` — PyYAML для чтения и записи `MANIFEST.yaml`.

## Матрица 2.1.5

| Цель | Управление | Что меняется |
|---|---|---|
| `yandexstation_2` | полное | `spectrum: true`: захват 89-пиксельного framebuffer-кольца |
| `yandexmini_2` | полное | `spectrum: true`, `led_color_profile: "red"`, яркость `1.25` |
| `plum`, `yandexplum` | полное | `spectrum: true`, яркость `1.25`, баланс белого `1.5/0.5/0.71` |
| `yandexmidi`, `cucumber` | полное | добавлен `--controls-config`; включены `native_player` и `alice_button` |
| `yandexmini` | полное, кроме кнопки Алисы | включены голос, ducking, индикация и native player; `spectrum: false` для шестизонного логотипа |
| `yandexmicro` | голос, ducking, громкость, индикация; native player при наличии Quasar CLI | без изменений |
| `mango` | голос, ducking, индикация | без изменений |
| `orion` | только звук | `spectrum: false`: большой экран |
| `bergamot` (Мини 3) | полный набор | включён controls-config; native volume через Luna >=0.2.25 для всех сборок на точной плате |
| `pickle` (Мини 3 Про) | только звук | без изменений; база `bergamot`, AArch64-приёмник и `libplist`, model ID `yandexmini_3_pro` |

`led_level` остаётся 200 у всех целей. Для аудио-профилей JSON всё равно
создаётся launcher'ом при первом запуске, но не передаётся супервизору.

Матрица 2.1.5 добавляет в конфиг `spectrum`, `spectrum_brightness`,
`spectrum_gain_r/g/b` и `led_color_profile`. Обложки включены на всех целях с
RGB-кольцом, включая `yandexstation_2`: у неё нет sysfs-кольца, поэтому
плагин на время сессии забирает 89-пиксельный `/dev/fb0` у `led-animator` и
возвращает его после отключения. Исключены только `yandexstation_3` и
`orion`. У `pickle` управление не передаётся, поэтому обложки начнут работать
только после квалификации AArch64-профиля.

## Сборка

```sh
python3 -m pip install -r release/yampkg/requirements.txt
python3 scripts/package_yampkg.py --check

python3 scripts/package_yampkg.py \
  --base /path/to/published/2.1.2 \
  --binary /path/to/station-airplay-2.1.2.upx \
  --version 2.1.5 --slim-runtime \
  --out dist/yampkg
```

`--base` — каталог с опубликованными пакетами вида
`station-airplay-2.1.0-<target>.yampkg` или `<target>.yampkg`. `--binary` —
ARMv7-супервизор; сборщик проверяет ELF32/EM_ARM. `--version` по умолчанию
берётся из `plugin.toml`. `--targets` позволяет собрать подмножество,
`--force` разрешает заменить существующий файл.

### Legacy-runtime оригинальной Mini

При `--slim-runtime` только `yandexmini` сохраняет из проверенной базы
`lib/ld-linux-armhf.so.3`, `libc`, `libm`, `libdl`, `libpthread`, `librt`,
`libresolv` и `libgcc_s`. Это необходимо для прошивки с glibc 2.23.
База должна содержать эти файлы и их правильные SHA256; отсутствие файла
останавливает сборку. Для такой сборки используйте полную базу 2.1.4,
а не slim-пакет 2.1.5/2.1.6. Системная `/lib` колонки не меняется.
Остальные цели продолжают использовать slim-runtime без второй libc.
Динамические runtime-бинарники Mini не упаковываются UPX: запуск через
явный `ld-linux` требует исходной dynamic section. Статический Go-супервизор
может оставаться UPX-сжатым — он запускается напрямую, без этого loader.

Сборщик пересчитывает хеши **всех** payload-файлов, проверяет точное
совпадение payload со списком манифеста и исполняемые права launcher,
бинарников, postinstall и loader. В provenance записаны хеши сохранённых
legacy-файлов. Отдельный скрипт перепаковки или ручной `chmod` не нужны.

```sh
python3 tests/test_yampkg_integrity.py
```

`--extra-dir` по умолчанию указывает на корень репозитория. Цель `pickle`
берёт оттуда `bin/shairport-sync-ap2-aarch64` и
`lib/aarch64/libplist-2.0.so.3`; эти файлы уже входят в universal `.yap`.
Другой каталог можно передать явно, если сборка идёт из распакованного
релиза.

## Что патчится

- `bin/station-airplay` — новый супервизор.
- `launch-airplay.sh` — из матрицы.
- `config/airplay-controls-v2.json` — из матрицы.
- `plugin.toml` — версия, список совместимых model ID и, для баз 2.0.9,
  узкие права `media.native` / `input.alice`.
- `description` в `MANIFEST.yaml` — берётся из `plugin.toml`. Сборщик
  отказывается собирать пакет, если в описании нет фразы
  `Требует Luna <версия>+.` из `LUNA_DEPENDENCY`.
- `MANIFEST.yaml` — target, версия, `depends: yam/luna >=0.2.26` и все
  `files_sha256`, включая добавленные AArch64-файлы.

Архив упаковывается `zstd -19`: формат и распаковка на колонке не меняются,
но итоговый `.yampkg` меньше, чем при уровне по умолчанию (`tar --zstd`).

Сборщик проверяет хеши базового пакета до правок и хеши результата после
упаковки, а также ELF-класс, архитектуру, launcher/config и наличие
добавленных файлов.

## Что не делается

- `arch` в манифесте наследуется от базового пакета. Релиз по-прежнему везёт
  один ARMv7-супервизор на все цели, как 2.0.9/2.1.0. Для `pickle` отдельно
  добавляется AArch64-приёмник, но не AArch64-супервизор.
- `postinstall` заменяется на `release/yampkg/airplay-postinstall.sh`:
  исправлен случай, когда у Luna ещё нет `settings.toml` и автозапуск
  плагина не срабатывает. Shairport Sync, библиотеки и лицензии не
  пересобираются.
- AArch64 shared-управление не квалифицировано: `pickle` остаётся
  аудио-профилем без `--controls-config`.
- Нет подписи, публикации в реестр и GitHub release. Для публикации используйте
  `yam-publish` и отдельную проверку readback.
- Нет автоматического повышения версии: `--version` задаётся явно.

## Проверки

`python3 scripts/package_yampkg.py --check` и
`python3 tests/test_package_yampkg.py` не требуют базовых пакетов, PyYAML или
сети. Они проверяют матрицу из 12 целей, launcher, JSON и `sh -n`. CI
запускает их на каждый PR. Полная сборка требует `zstd`, GNU `tar`, PyYAML,
AArch64-файлы runtime и базовый релиз.

### Orion: ARMhf runtime

При сборке `orion` передайте `scripts/package_yampkg.py` параметр `--orion-runtime station-airplay-2.0.9-orion.yampkg`. Архив доступен в [релизе v2.0.9](https://github.com/ryysel/Station-airplay/releases/tag/v2.0.9). Скрипт проверяет закреплённый SHA-256 и включает библиотеки в приватный каталог пакета. На прошивках с системным ARMhf-загрузчиком сохраняется штатный путь; на AArch64 без него используются приватный загрузчик и неупакованные дочерние ELF.
