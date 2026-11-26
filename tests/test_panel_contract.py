"""Static requirements from Luna docs/WEB_PANELS.md; no host CSS is bundled."""
from html.parser import HTMLParser
from pathlib import Path
import re
import subprocess
import tempfile

root=Path(__file__).resolve().parents[1]
page=(root/"panel/index.html").read_text()
class Contract(HTMLParser):
    def __init__(self):
        super().__init__()
        self.ids=set()
        self.links=[]
        self.scripts=[]
        self.inline=False
    def handle_starttag(self,tag,attrs):
        a=dict(attrs)
        assert tag not in ("style","nav","header"),tag
        assert "style" not in a,(tag,a)
        if "id" in a:
            assert a["id"] not in self.ids,a["id"]
            self.ids.add(a["id"])
        if tag=="link": self.links.append(a.get("href"))
        if tag=="script":
            assert not a.get("src"),"scripts must be inline"
            self.inline=True
    def handle_endtag(self,tag):
        if tag=="script": self.inline=False
    def handle_data(self,data):
        if self.inline: self.scripts.append(data)
c=Contract()
c.feed(page)
assert c.links==["/luna-panel.css"],c.links
assert not re.search(r"\.style\b|:root\s*\{|https?://(?!www\.w3\.org/2000/svg)",page)
assert "c[key]??" in page,"explicit zero must not fall back to defaults"
assert "throw Error($('owner').textContent||" in page,"transport error must remain visible"
assert "luna-theme" in page and "prefers-color-scheme:dark" in page
assert '<main class="wrap">' in page and 'class="head"' in page
assert re.search(r'<label[^>]*hidden[^>]*>Область настройки',page)
assert {"level","retry-save","dirty","live-strip","live-bands","eq-on","presets","preview","stop"}<=c.ids
assert "currentLEDLevel" in (root/"source/controls.go").read_text()
assert "input.tap, input.spectrumLevel" in (root/"source/display.go").read_text()
with tempfile.TemporaryDirectory() as d:
    path=Path(d)/"panel-inline.js"
    path.write_text("\n".join(c.scripts))
    subprocess.run(["node","--check",str(path)],check=True)
subprocess.run(["node",str(root/"panel/test-live-view.cjs"),str(root/"panel/index.html")],check=True)
print("LUNA_WEB_PANEL_CONTRACT_PASS: shared CSS only, theme, components, API, live telemetry")
