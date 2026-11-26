from pathlib import Path
import re

here = Path(__file__).resolve().parent
page = (here / 'calibration-base.html').read_text()
assert page.count('/* LIVE_VIEW_MODULE */') == 1
page = page.replace('/* LIVE_VIEW_MODULE */', (here / 'live-view.js').read_text())
assert page.count('/* AUTOSAVE_MODULE */') == 1
page = page.replace('/* AUTOSAVE_MODULE */', (here / 'autosave.js').read_text())
transport = (here / 'transport.js').read_text()
page, count = re.subn(r"async function api\(path,method='GET',body\)\{[^\n]+\}", lambda _:transport, page, count=1)
assert count == 1
page = page.replace('<option value="luna">Luna: общий LED backend, без AirPlay</option>', '')
page = page.replace('<label class="fld">Область настройки <select id="scope" class="in">', '<label class="fld hid" hidden>Область настройки <select id="scope" class="in">')
page = page.replace('Luna · Цвета и звук', 'AirPlay · Цвета и звук')
page = page.replace('LUNA / ЛОКАЛЬНАЯ КАЛИБРОВКА', 'AIRPLAY / ПАНЕЛЬ ПЛАГИНА')
page = page.replace('RGB-коррекция общей области Luna применяется сразу к её LED-клиентам.',
                    'Общая калибровка Luna в эту панель не входит.')
page = page.replace('if(auto&&scope===', 'if(false&&auto&&scope===')
page = page.replace('poll=setInterval(status,1500)', 'poll=setInterval(()=>{if(!busy)status();},3000)')
page = page.replace('Пресеты хранятся на колонке.', 'Пресеты хранятся в данных AirPlay на колонке.')
page = page.replace('luna-calibration.json', 'airplay-calibration.json')
page = page.replace('Проверьте сборку Luna / AirPlay и выбранную область',
                    'Включите AirPlay, AirPlay Panel и AirPlay Panel Command')
page = page.replace('Если закрыть вкладку, тест закончится примерно через 4 с.',
                    'Если закрыть вкладку или helper не успеет продлить lease, тест закончится примерно через 4 с. Не открывайте несколько панелей одновременно.')
(here / 'index.html').write_text(page)
