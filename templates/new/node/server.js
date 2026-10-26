import http from "node:http";

const port = process.env.PORT || 8080;

const server = http.createServer((req, res) => {
  if (req.url === "/healthz") {
    res.writeHead(200, { "Content-Type": "text/plain" });
    return res.end("ok\n");
  }
  res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
  res.end(`<!doctype html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.App}}</title>
<body style="font: 18px/1.5 system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; padding: 0 1rem">
<h1>{{.App}}</h1>
<p>It's live. Edit <code>server.js</code> and push to deploy.</p>
</body>`);
});

server.listen(port, "0.0.0.0", () => console.log(`{{.App}} listening on :${port}`));
