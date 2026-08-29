package gateway

import (
	"bytes"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
)

const (
	plexWebScriptPath   = "/:/plex-gateway/web-direct-play-v2.js"
	plexWebBodyMaxBytes = 2 << 20
)

var (
	plexWebScriptTag = []byte(`<script src="` + plexWebScriptPath + `"></script>`)
	plexWebScript    = []byte(`(() => {
  "use strict";

  const marker = "__plexGatewayDirectPlayV2";
  if (window[marker]) return;
  Object.defineProperty(window, marker, { value: true });

  const mediaPrototype = window.HTMLMediaElement && window.HTMLMediaElement.prototype;
  if (!mediaPrototype) return;

  const source = Object.getOwnPropertyDescriptor(mediaPrototype, "src");
  const crossOrigin = Object.getOwnPropertyDescriptor(mediaPrototype, "crossOrigin");
  const setAttribute = window.Element.prototype.setAttribute;
  const removeAttribute = window.Element.prototype.removeAttribute;
  const preservedCrossOrigin = new WeakMap();

  const isGatewayCloudPart = value => {
    if (!value) return false;
    try {
      const target = new URL(String(value), document.baseURI);
      const path = target.pathname.toLowerCase();
      return target.origin === window.location.origin &&
        path.startsWith("/library/parts/") &&
        (path.endsWith("/file") || path.endsWith(".strm"));
    } catch (_) {
      return false;
    }
  };

  const currentSource = element => {
    const attribute = element.getAttribute("src");
    if (attribute) return attribute;
    return source && source.get ? source.get.call(element) : "";
  };

  const suppressPartCrossOrigin = (element, candidate) => {
    if (!isGatewayCloudPart(candidate)) return false;
    if (!preservedCrossOrigin.has(element)) {
      preservedCrossOrigin.set(element, element.getAttribute("crossorigin"));
    }
    removeAttribute.call(element, "crossorigin");
    return true;
  };

  const restorePartCrossOrigin = element => {
    if (!preservedCrossOrigin.has(element)) return;
    const value = preservedCrossOrigin.get(element);
    preservedCrossOrigin.delete(element);
    if (value === null) {
      removeAttribute.call(element, "crossorigin");
    } else {
      setAttribute.call(element, "crossorigin", value);
    }
  };

  if (source && source.configurable && source.get && source.set) {
    Object.defineProperty(mediaPrototype, "src", {
      configurable: true,
      enumerable: source.enumerable,
      get: source.get,
      set(value) {
        if (!suppressPartCrossOrigin(this, value)) {
          restorePartCrossOrigin(this);
        }
        source.set.call(this, value);
      }
    });
  }

  if (crossOrigin && crossOrigin.configurable && crossOrigin.get && crossOrigin.set) {
    Object.defineProperty(mediaPrototype, "crossOrigin", {
      configurable: true,
      enumerable: crossOrigin.enumerable,
      get: crossOrigin.get,
      set(value) {
        if (isGatewayCloudPart(currentSource(this))) {
          preservedCrossOrigin.set(this, value == null ? null : String(value));
          removeAttribute.call(this, "crossorigin");
          return;
        }
        preservedCrossOrigin.delete(this);
        crossOrigin.set.call(this, value);
      }
    });
  }

})();
`)
)

// plexWebCompatibility changes only the Plex Web shell and its versioned helper
// script. Media requests remain ordinary Part requests, so cloud bytes still
// travel directly from the final media origin to the browser after the 302.
type plexWebCompatibility struct {
	enabled      bool
	logger       *slog.Logger
	maxBodyBytes int64
}

func newPlexWebCompatibility(enabled bool, logger *slog.Logger) *plexWebCompatibility {
	return &plexWebCompatibility{enabled: enabled, logger: logger, maxBodyBytes: plexWebBodyMaxBytes}
}

func (compatibility *plexWebCompatibility) prepareProxyRequest(request *httputil.ProxyRequest) {
	if compatibility == nil || !compatibility.enabled || request == nil || !isPlexWebShellRequest(request.In) {
		return
	}
	// The shell is small and modified once. Requesting identity avoids buffering
	// browser-selected Brotli or Zstandard encodings that the Go standard library
	// cannot rewrite without adding another codec dependency.
	request.Out.Header.Set("Accept-Encoding", "identity")
	for _, name := range []string{"If-Match", "If-Modified-Since", "If-None-Match", "If-Unmodified-Since", "Range"} {
		request.Out.Header.Del(name)
	}
}

func (compatibility *plexWebCompatibility) modifyResponse(response *http.Response) error {
	if compatibility == nil || !compatibility.enabled || !isPlexWebShellResponse(response) {
		return nil
	}

	original := response.Body
	body, err := io.ReadAll(io.LimitReader(original, compatibility.maxBodyBytes+1))
	if err != nil || int64(len(body)) > compatibility.maxBodyBytes {
		response.Body = &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}
		compatibility.logSkip(response, "body_unavailable")
		return nil
	}
	_ = original.Close()

	if bytes.Contains(body, plexWebScriptTag) {
		response.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	headEnd := bytes.Index(bytes.ToLower(body), []byte("</head>"))
	if headEnd < 0 {
		response.Body = io.NopCloser(bytes.NewReader(body))
		compatibility.logSkip(response, "head_missing")
		return nil
	}

	modified := make([]byte, 0, len(body)+len(plexWebScriptTag)+1)
	modified = append(modified, body[:headEnd]...)
	modified = append(modified, plexWebScriptTag...)
	modified = append(modified, '\n')
	modified = append(modified, body[headEnd:]...)

	for _, name := range []string{
		"Accept-Ranges", "Content-Length", "Content-MD5", "Content-Range", "Digest", "ETag", "Last-Modified", "Trailer",
	} {
		response.Header.Del(name)
	}
	cachePolicy := strings.Join(response.Header.Values("Cache-Control"), ", ")
	if !hasCacheDirective(cachePolicy, "no-store") && !hasCacheDirective(cachePolicy, "no-cache") {
		if strings.TrimSpace(cachePolicy) == "" {
			cachePolicy = "no-cache"
		} else {
			cachePolicy += ", no-cache"
		}
		response.Header.Set("Cache-Control", cachePolicy)
	}
	response.Header.Set("Content-Length", strconv.Itoa(len(modified)))
	response.Header.Set("X-Plex-Gateway-Web-Compat", "direct-play-v2")
	response.ContentLength = int64(len(modified))
	response.Trailer = nil
	response.Body = io.NopCloser(bytes.NewReader(modified))
	return nil
}

func (compatibility *plexWebCompatibility) serveScript(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	writer.Header().Set("Content-Length", strconv.Itoa(len(plexWebScript)))
	writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(plexWebScript)
	}
}

func (compatibility *plexWebCompatibility) logSkip(response *http.Response, reason string) {
	if compatibility.logger == nil || response == nil || response.Request == nil || response.Request.URL == nil {
		return
	}
	compatibility.logger.Warn("plex_web_compat_skipped", "path", response.Request.URL.EscapedPath(), "reason", reason)
}

func isPlexWebShellRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet || request.URL == nil {
		return false
	}
	return request.URL.Path == "/web/" || request.URL.Path == "/web/index.html"
}

func isPlexWebShellResponse(response *http.Response) bool {
	if response == nil || response.Request == nil || !isPlexWebShellRequest(response.Request) ||
		response.StatusCode != http.StatusOK || response.Body == nil {
		return false
	}
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "text/html")
}

type replayReadCloser struct {
	io.Reader
	io.Closer
}
