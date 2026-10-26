from fastapi import FastAPI
from fastapi.responses import HTMLResponse

app = FastAPI()


@app.get("/healthz")
def healthz():
    return {"ok": True}


@app.get("/", response_class=HTMLResponse)
def home():
    return """<!doctype html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.App}}</title>
<body style="font: 18px/1.5 system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; padding: 0 1rem">
<h1>{{.App}}</h1>
<p>It's live. Edit <code>main.py</code> and push to deploy.</p>
</body>"""
