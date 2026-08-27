// Permalinks carry the whole program in the URL fragment.
//
// The fragment is never sent to the server, so shared links leave no trace in
// request logs and the playground needs no database, no snippet IDs, and no
// retention policy. The cost is URL length, which deflate keeps manageable:
// the largest bundled sample is ~6KB of source and lands near 1.5KB encoded.

const PREFIX = "#src=";

/** Encode source into a URL fragment. Falls back to plain base64url where the
 *  Compression Streams API is unavailable. */
export async function encodeSource(source) {
  const bytes = new TextEncoder().encode(source);

  if (typeof CompressionStream === "function") {
    try {
      const packed = await pipeThrough(bytes, new CompressionStream("deflate-raw"));
      return PREFIX + toBase64Url(packed);
    } catch {
      // Fall through to the uncompressed form rather than failing to share.
    }
  }
  return PREFIX + toBase64Url(bytes);
}

/** Read a program out of the current location's fragment, or null. */
export async function decodeFragment(hash = window.location.hash) {
  if (!hash.startsWith(PREFIX)) return null;

  const encoded = hash.slice(PREFIX.length);
  if (!encoded) return null;

  let bytes;
  try {
    bytes = fromBase64Url(encoded);
  } catch {
    return null;
  }

  if (typeof DecompressionStream === "function") {
    try {
      const raw = await pipeThrough(bytes, new DecompressionStream("deflate-raw"));
      return new TextDecoder().decode(raw);
    } catch {
      // Not deflate — most likely a link made by the fallback path above.
    }
  }

  try {
    // Reject bytes that aren't valid UTF-8 rather than rendering replacement
    // characters, so a corrupted link is treated as absent.
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    return null;
  }
}

async function pipeThrough(bytes, transform) {
  const stream = new Blob([bytes]).stream().pipeThrough(transform);
  const buffer = await new Response(stream).arrayBuffer();
  return new Uint8Array(buffer);
}

function toBase64Url(bytes) {
  let binary = "";
  // Chunked to stay well clear of the argument-count limit on apply().
  const CHUNK = 0x8000;
  for (let i = 0; i < bytes.length; i += CHUNK) {
    binary += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK));
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function fromBase64Url(encoded) {
  const padded = encoded.replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}
