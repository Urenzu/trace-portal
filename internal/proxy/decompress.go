package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"strings"
)

// decodeBody returns the readable form of a captured body.
//
// Real clients negotiate compression — Claude Code and the SDKs send an
// Accept-Encoding header, which ReverseProxy forwards, and the API answers with
// a gzipped stream. Because the proxy forwards that header verbatim it also
// receives the compressed bytes, so everything downstream (usage parsing, tool
// extraction, the stored payload) has to decode them first.
//
// Only the capture path decodes. The bytes handed to the client are never
// touched, so the proxy stays transparent.
//
// An encoding we cannot decode is returned unchanged with ok=false, which
// degrades the trace to its narrow fields rather than failing the request.
func decodeBody(body []byte, contentEncoding string) (decoded []byte, ok bool) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return body, true

	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body, false
		}
		defer zr.Close()
		// A stream cut off mid-flight is expected: the client may have
		// disconnected. Keep whatever decoded cleanly up to that point.
		out, err := io.ReadAll(zr)
		if len(out) == 0 && err != nil {
			return body, false
		}
		return out, true

	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return body, false
		}
		defer zr.Close()
		out, err := io.ReadAll(zr)
		if len(out) == 0 && err != nil {
			return body, false
		}
		return out, true

	default:
		// br and zstd would need third-party decoders; the narrow fields still
		// get recorded, and the raw payload is still stored verbatim.
		return body, false
	}
}
