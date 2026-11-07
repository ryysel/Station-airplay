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

## Матрица 2.1.0 → 2.1.1

| Цель | Управление | Что меняется |
|---|---|---|
| `yandexstation_2`, `yandexmini_2`, `plum`, `yandexplum` | полное | без изменений |
| `yandexmidi`, `cucumber` | полное | добавлен `--controls-config`; включены `native_player` и `alice_button` |
| `yandexmini` | только громкость | добавлен `--controls-config`; остальные флаги выключены |
| `yandexmicro` | голос, ducking, громкость, индикация; native player при наличии Quasar CLI | без изменений |
| `mango` | голос, ducking, индикация | без изменений |
| `orion` | только звук | без изменений |
| `bergamot` (Мини 3) | полный набор | включён controls-config; нативная громкость требует исправления Luna для проверенной прошивки |
| `pickle` (Мини 3 Про) | только звук | новая цель: база `bergamot`, AArch64-приёмник и `libplist`, model ID `yandexmini_3_pro` |

`led_level` остаётся 200 у всех целей. Для аудио-профилей JSON всё равно
создаётся launcher'ом при первом запуске, но не передаётся супервизору.

## Сборка

```sh
python3 -m pip install -r release/yampkg/requirements.txt
python3 scripts/package_yampkg.py --check

python3 scripts/package_yampkg.py \
  --base /path/to/published/2.1.0 \
  --binary /path/to/station-airplay-2.1.0.upx \
  --version 2.1.1 \
  --out dist/yampkg
```

`--base` — каталог с опубликованными пакетами вида
`station-airplay-2.1.0-<target>.yampkg` или `<target>.yampkg`. `--binary` —
ARMv7-супервизор; сборщик проверяет ELF32/EM_ARM. `--version` по умолчанию
берётся из `plugin.toml`. `--targets` позволяет собрать подмножество,
`--force` разрешает заменить существующий файл.

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
- `MANIFEST.yaml` — target, версия, `depends: yam/luna >=0.2.24` и все
  `files_sha256`, включая добавленные AArch64-файлы.

Сборщик проверяет хеши базового пакета до правок и хеши результата после
упаковки, а также ELF-класс, архитектуру, launcher/config и наличие
добавленных файлов.

## Что не делается

- `arch` в манифесте наследуется от базового пакета. Релиз по-прежнему везёт
  один ARMv7-супервизор на все цели, как 2.0.9/2.1.0. Для `pickle` отдельно
  добавляется AArch64-приёмник, но не AArch64-супервизор.
- Не пересобираются Shairport Sync, библиотеки, `postinstall` и лицензии.
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
