export async function authenticateOperator(request, tokenDigest) {
  if (typeof tokenDigest !== "string" || !/^[a-fA-F0-9]{64}$/.test(tokenDigest)) {
    return false;
  }
  const authorization = request.headers.get("Authorization") ?? "";
  const match = /^Bearer ([^\s]+)$/i.exec(authorization);
  if (match === null) {
    return false;
  }
  const actual = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(match[1])));
  let difference = 0;
  for (let index = 0; index < actual.length; index += 1) {
    const expected = Number.parseInt(tokenDigest.slice(index * 2, index * 2 + 2), 16);
    difference |= actual[index] ^ expected;
  }
  return difference === 0;
}
