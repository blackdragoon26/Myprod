export default function handler(request, response) {
  if (request.method !== "GET") {
    response.setHeader("Allow", "GET");
    response.status(405).json({ error: "method not allowed" });
    return;
  }

  const publishableKey = String(process.env.NEXT_PUBLIC_CLERK_PUBLISHABLE_KEY || "").trim();
  if (!/^pk_(test|live)_[A-Za-z0-9_-]+$/.test(publishableKey)) {
    response.status(503).json({ error: "Clerk sign-in is not configured" });
    return;
  }

  let frontendAPI;
  try {
    const encoded = publishableKey.replace(/^pk_(test|live)_/, "");
    const decoded = Buffer.from(encoded, "base64url").toString("utf8");
    if (!decoded.endsWith("$")) throw new Error("invalid frontend API marker");
    frontendAPI = decoded.slice(0, -1);
    const parsed = new URL(`https://${frontendAPI}`);
    if (parsed.hostname !== frontendAPI || parsed.pathname !== "/") throw new Error("invalid frontend API");
  } catch {
    response.status(503).json({ error: "Clerk sign-in configuration is invalid" });
    return;
  }

  response.setHeader("Cache-Control", "no-store");
  response.status(200).json({ publishableKey, frontendAPI });
}
