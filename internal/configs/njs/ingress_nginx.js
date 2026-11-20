// Variables of ingress-nginx that open-source nginx has no directive for.

const unreserved = /[A-Za-z0-9\-._~]/;

// escapedRequestURI is $escaped_request_uri: $request_uri escaped as
// set_escape_uri escapes it in ingress-nginx, every byte but A-Z a-z 0-9 - . _ ~
// as %XX.
function escapedRequestURI(r) {
    const uri = r.rawVariables.request_uri;
    if (!uri) {
        return '';
    }
    let escaped = '';
    for (let i = 0; i < uri.length; i++) {
        const c = String.fromCharCode(uri[i]);
        if (uri[i] < 0x80 && unreserved.test(c)) {
            escaped += c;
        } else {
            escaped += '%' + (uri[i] < 0x10 ? '0' : '') + uri[i].toString(16).toUpperCase();
        }
    }
    return escaped;
}

export default { escapedRequestURI };
