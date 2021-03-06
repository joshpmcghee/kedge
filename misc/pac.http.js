function FindProxyForURL(url, host) {
    // A request over http, where the domain has the suffix '.internal.improbable.io' and
    // an optional port.
    var regexp = /^http:\/\/[^\/]+\.local(\:[\d]+)?\//
    if (regexp.test(url)) {
        return "PROXY localhost:8080"
    }
    return "DIRECT"
}
