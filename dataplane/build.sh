#!/bin/sh
#
# Lowers the data plane with meta and builds it as an nginx module.
#
#   ./build.sh              lower, build nginx and the module
#   ./build.sh module       only the module, against an nginx built here once
#   ./build.sh test         and then run the tests
#   ./build.sh curl         only the static libcurl (in an older libc's
#                           container, say, for the module's symbol versions)
#   ./build.sh clean        throw the build directory away
#
# Where things are, all overridable:
#
#   META_ROOT       a metalanguage checkout (../../metalanguage)
#   META            the meta binary ($META_ROOT/meta)
#   NGINX_SOURCE    an nginx source tree to build from. Without it the pinned
#                   release below is downloaded once into build/.
#
# **The module is built for the nginx the image runs.** NGINX Ingress
# Controller's image installs nginx from nginx.org's packages, and those are
# built with --with-compat. A module built here from the same release with
# --with-compat therefore agrees with that binary about
# NGX_MODULE_SIGNATURE, and `load_module` takes it. The nginx built beside it
# is for the tests, which need something to run.
#
# The module is named ngx_http_wxingress_module.so, which is the file the
# controller's main template loads. Inside it is meta's ngx_http_meta_module:
# nginx reads module names from the object, not from its file name.

set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
build="$here/build"
addon="$build/addon"

root=${META_ROOT:-$here/../../metalanguage}
meta=${META:-$root/meta}

# the version NGINX_OSS_VERSION in build/Dockerfile names
NGINX_VERSION=1.31.10
NGINX_SHA256=a9b690957fca84abdae8a9b52d66b1bb459282d7f01fe4b94b498ed8e977bdb3
NGINX_URL=https://nginx.org/download/nginx-$NGINX_VERSION.tar.gz

# libcurl, which meta's fetch drives (JWK sets, OIDC discovery), linked into
# the module as a static library of HTTP and HTTPS alone: the distribution's
# libcurl brings some thirty libraries - GnuTLS, Kerberos, LDAP, SASL, SSH,
# RTMP, IDN - that every nginx process would map, a few MB of each pod's
# memory for protocols no Policy speaks. CURL_SHARED=1 links the system's
# libcurl instead.
CURL_VERSION=8.22.0
CURL_SHA256=d54dd598bf05927a726deb38df31c6a255ba83ff1de57c5d1464dac3ed8f44a1
CURL_URL=https://curl.se/download/curl-$CURL_VERSION.tar.gz

say() { printf '%s\n' "$*"; }

mode=${1:-}

if [ "$mode" = "clean" ]; then
  rm -rf "$build"
  say "gone"
  exit 0
fi

# ------------------------------------------------------------------- libcurl

curl="$build/curl-$CURL_VERSION"

if [ "${CURL_SHARED:-0}" != 1 ]; then
  if [ ! -f "$curl/lib/libcurl.a" ]; then
    say "building libcurl $CURL_VERSION"
    rm -rf "$build/curl-src"
    mkdir -p "$build/curl-src"
    curl -sSfL -o "$build/curl.tar.gz" "$CURL_URL"
    printf '%s  %s\n' "$CURL_SHA256" "$build/curl.tar.gz" | sha256sum -c - >/dev/null
    tar xzf "$build/curl.tar.gz" -C "$build/curl-src" --strip-components=1
    rm -f "$build/curl.tar.gz"
    (
      cd "$build/curl-src"
      # the CA bundle and directory of the image (Debian), which fetch
      # replaces with a Policy's own CA where one is given
      ./configure --prefix="$curl" --disable-shared --enable-static --with-pic \
          --with-openssl --with-zlib --enable-threaded-resolver \
          --with-ca-bundle=/etc/ssl/certs/ca-certificates.crt --with-ca-path=/etc/ssl/certs \
          --disable-ftp --disable-file --disable-ldap --disable-ldaps --disable-rtsp \
          --disable-dict --disable-telnet --disable-tftp --disable-pop3 --disable-imap \
          --disable-smb --disable-smtp --disable-gopher --disable-mqtt --disable-ipfs \
          --disable-websockets --disable-manual --disable-docs --disable-ntlm \
          --disable-kerberos-auth --disable-negotiate-auth --disable-aws --disable-unix-sockets \
          --without-libpsl --without-libidn2 --without-nghttp2 --without-nghttp3 --without-ngtcp2 \
          --without-libssh2 --without-libssh --without-librtmp --without-brotli --without-zstd \
          --without-gssapi --without-libgsasl >"$build/curl-configure.log" 2>&1 &&
      make -j"$(nproc 2>/dev/null || echo 4)" >"$build/curl-make.log" 2>&1 &&
      make install >"$build/curl-install.log" 2>&1
    ) || { tail -5 "$build/curl-configure.log" "$build/curl-make.log" 2>/dev/null; exit 1; }
    rm -rf "$build/curl-src"
  fi
fi

if [ "$mode" = "curl" ]; then
  exit 0
fi

[ -x "$meta" ] || { say "no meta at $meta"; exit 2; }

# ------------------------------------------------------------------ lowering

say "lowering"

rm -rf "$addon"
mkdir -p "$build"

# meta lowers one program into a module: the files main.c includes from src/
# with `#include "<file>.c"` (jwt.c) are put in its place first
awk -v dir="$here/src" '/^#include "[a-z]+\.c"$/ { f = $2; gsub(/"/, "", f); while ((getline line < (dir "/" f)) > 0) print line; close(dir "/" f); next } { print }' \
  "$here/src/main.c" >"$build/main.c"
"$meta" -s -module "$addon" "$build/main.c" >"$build/lower.log" 2>&1 || {
  cat "$build/lower.log"
  say "lowering failed, see $build/lower.log"
  exit 1
}

# What meta's analyses say - ownership, blocking calls on the loop, what it
# could not resolve - is shown on every build, not only when lowering fails:
# a note that nobody reads is a note that is never acted on.
cat "$build/lower.log"

if [ "${CURL_SHARED:-0}" != 1 ]; then
  # the addon meta wrote names -lcurl: the static one instead, its headers
  # first, and OpenSSL and zlib from the nginx that loads the module
  sed -i -e "s|ngx_module_incs=\"|ngx_module_incs=\"$curl/include |" \
         -e "s|-lcurl|$curl/lib/libcurl.a -lssl -lcrypto -lz|" "$addon/config"
fi

# ------------------------------------------------------------------- nginx

source=${NGINX_SOURCE:-}

if [ -z "$source" ]; then
  source="$build/nginx-$NGINX_VERSION"

  if [ ! -f "$source/src/core/nginx.h" ]; then
    say "fetching nginx $NGINX_VERSION"
    curl -sSfL -o "$build/nginx.tar.gz" "$NGINX_URL"
    printf '%s  %s\n' "$NGINX_SHA256" "$build/nginx.tar.gz" | sha256sum -c - >/dev/null
    tar xzf "$build/nginx.tar.gz" -C "$build"
    rm -f "$build/nginx.tar.gz"
  fi
fi

prefix="$build/nginx"

# gcc 15 warns about nginx's unterminated string initializers, and nginx
# builds with -Werror. An older gcc - the meta image's is 13 - does not know
# the warning and refuses an option that names it, so it is only passed to a
# compiler that takes it.
ccopt=
if printf 'int main(void){return 0;}\n' |
     ${CC:-cc} -Werror -Wno-error=unterminated-string-initialization \
       -x c -o /dev/null - >/dev/null 2>&1; then
  ccopt=-Wno-error=unterminated-string-initialization
fi

# Configured once: configure takes the addon's path, and the addon is
# rewritten in place by every lowering, so `make` picks the new one up.
# The libraries of the addon are written into objs/Makefile, so another set
# of them - the static libcurl or not - is configured anew.
libs=$(grep ngx_module_libs= "$addon/config")
if [ ! -f "$source/objs/Makefile" ] || [ ! -f "$build/configured" ] ||
   [ "$(cat "$build/configured")" != "$libs" ]; then
  say "configuring nginx"
  (
    cd "$source"
    ./configure --prefix="$prefix" --with-compat --with-threads \
        --with-http_ssl_module --with-http_v2_module \
        --with-http_realip_module --with-http_stub_status_module \
        --with-stream --with-stream_ssl_module \
        ${ccopt:+"--with-cc-opt=$ccopt"} \
        --add-dynamic-module="$addon" >"$build/configure.log" 2>&1
  ) || { tail -5 "$build/configure.log"; exit 1; }
  printf '%s\n' "$libs" >"$build/configured"
fi

# nginx's Makefile does not know that meta's runtime headers are inputs - of
# the module and of the runtime's own sources, which define what the module
# uses (http_t): an object of either built with other headers is another layout
rm -f "$source"/objs/addon/*/ngx_http_meta_module.o "$source/objs/ngx_http_meta_module.so" \
  "$source"/objs/meta_abi.o "$source"/objs/meta_tasks.o "$source"/objs/meta_http.o

if [ "$mode" = "module" ] && [ -x "$prefix/sbin/nginx" ]; then
  say "building the module"
  make -C "$source" -j"$(nproc 2>/dev/null || echo 4)" modules >"$build/make.log" 2>&1 ||
    { grep -E "error" "$build/make.log" | head -10; exit 1; }
else
  say "building nginx and the module"
  make -C "$source" -j"$(nproc 2>/dev/null || echo 4)" >"$build/make.log" 2>&1 ||
    { grep -E "error" "$build/make.log" | head -10; exit 1; }
  make -C "$source" install >"$build/install.log" 2>&1
fi

if grep -q "warning:" "$build/make.log"; then
  grep -A3 "warning:" "$build/make.log" | grep -v "^--$" | head -20
  say "the build has warnings, see $build/make.log"
  exit 1
fi

# The image is Debian trixie, whose libc is 2.41: a module asking for a newer
# symbol version than that would build here and refuse to load there.
newest=$(objdump -T "$source/objs/ngx_http_meta_module.so" 2>/dev/null |
         sed -n 's/.*GLIBC_\([0-9.]*\).*/\1/p' | sort -V | tail -1)

if [ -n "$newest" ] && [ "$(printf '%s\n2.41\n' "$newest" | sort -V | tail -1)" != "2.41" ]; then
  say "the module needs glibc $newest, and the image has 2.41"
  exit 1
fi

mkdir -p "$prefix/modules"
cp "$source/objs/ngx_http_meta_module.so" "$build/ngx_http_wxingress_module.so"
cp "$source/objs/ngx_http_meta_module.so" "$prefix/modules/ngx_http_wxingress_module.so"

say "built $build/ngx_http_wxingress_module.so"

if [ "$mode" = "test" ]; then
  NGINX="$prefix/sbin/nginx" MODULE="$build/ngx_http_wxingress_module.so" \
      "$here/tests/upstream-api.sh" || exit 1
  NGINX="$prefix/sbin/nginx" MODULE="$build/ngx_http_wxingress_module.so" \
      "$here/tests/certificates.sh"
fi
