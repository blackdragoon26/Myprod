const TARGETS = [
  { label: "HTTP", url: "http://api.sankalpjha.dev/" },
  { label: "HTTPS", url: "https://api.sankalpjha.dev/" },
];

export default async function handler(request, response) {
  if (request.method !== "GET") {
    response.setHeader("Allow", "GET");
    response.status(405).json({ error: "method not allowed" });
    return;
  }

  const checks = await Promise.all(TARGETS.map(checkTarget));
  response.setHeader("Cache-Control", "no-store");
  response.status(200).json({
    ok: checks.every((check) => check.ok),
    checkedAt: new Date().toISOString(),
    checks,
  });
}

async function checkTarget(target) {
  const started = Date.now();
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 6000);
  try {
    const res = await fetch(target.url, {
      method: "GET",
      redirect: "manual",
      signal: controller.signal,
      headers: { "user-agent": "poolctl-vercel-smoke/1.0" },
    });
    const body = await res.text();
    return {
      label: target.label,
      url: target.url,
      ok: res.status >= 200 && res.status < 400,
      status: `${res.status} ${res.statusText || ""}`.trim(),
      latencyMs: Date.now() - started,
      tls: target.url.startsWith("https://") ? "verified by runtime" : "",
      sample: body.slice(0, 160),
    };
  } catch (error) {
    return {
      label: target.label,
      url: target.url,
      ok: false,
      status: error.name === "AbortError" ? "timeout" : error.message,
      latencyMs: Date.now() - started,
      tls: "",
      sample: "",
    };
  } finally {
    clearTimeout(timeout);
  }
}
