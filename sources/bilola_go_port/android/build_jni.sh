#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd "$(dirname "$0")/.." && pwd)"
ndk="${ANDROID_NDK_ROOT:-/opt/android-sdk/ndk/26.1.10909125}"
sysroot="$ndk/toolchains/llvm/prebuilt/linux-x86_64/sysroot"
output_dir="${1:-$project_dir/build/android-jni}"

test -f "$sysroot/usr/include/jni.h"
mkdir -p "$output_dir/arm64-v8a" "$output_dir/armeabi-v7a" \
         "$output_dir/x86_64" "$output_dir/x86"

build_abi() {
    local goarch="$1"
    local goarm="$2"
    local triple="$3"
    local library_dir="$4"
    local destination="$5"
    # The host is arm64, so use its native Clang while linking against the
    # Android NDK sysroot.  -fuse-ld is a link-only option and makes cgo's
    # compile probes fail under -Werror when it is included in CC.
    local compiler="/usr/bin/clang --target=${triple}23 --sysroot=${sysroot}"
    local clang_rt="$ndk/toolchains/llvm/prebuilt/linux-x86_64/lib/clang/17/lib/linux"
    local resource_dir="$ndk/toolchains/llvm/prebuilt/linux-x86_64/lib/clang/17"
    local compiler="${compiler} -resource-dir=${resource_dir}"
    # c-shared uses the runtime's dynamic-loader helpers.  Android exposes
    # those through libdl, and the dependency must be recorded explicitly;
    # otherwise the APK installs but System.loadLibrary fails on real devices
    # with "cannot locate symbol dlopen".
    local linker_flags="--target=${triple}23 --sysroot=${sysroot} -resource-dir=${resource_dir} -fuse-ld=lld -rtlib=compiler-rt -L${sysroot}/usr/lib/${library_dir}/23 -L${clang_rt} -ldl"

    CGO_ENABLED=1 GOOS=android GOARCH="$goarch" GOARM="$goarm" \
        CC="$compiler" CGO_CFLAGS="--sysroot=${sysroot} -resource-dir=${resource_dir}" \
        CGO_LDFLAGS="$linker_flags" \
        go build -buildvcs=false -trimpath -buildmode=c-shared \
        -ldflags='-s -w' -o "$destination/libbhttpjni.so" ./android/jni
    rm -f "$destination/libbhttpjni.h"
}

cd "$project_dir"
build_abi arm64 '' aarch64-linux-android aarch64-linux-android \
    "$output_dir/arm64-v8a"
build_abi arm 7 armv7a-linux-androideabi arm-linux-androideabi \
    "$output_dir/armeabi-v7a"
build_abi amd64 '' x86_64-linux-android x86_64-linux-android \
    "$output_dir/x86_64"
build_abi 386 '' i686-linux-android i686-linux-android \
    "$output_dir/x86"

file "$output_dir/arm64-v8a/libbhttpjni.so" \
    "$output_dir/armeabi-v7a/libbhttpjni.so" \
    "$output_dir/x86_64/libbhttpjni.so" \
    "$output_dir/x86/libbhttpjni.so"
sha256sum "$output_dir/arm64-v8a/libbhttpjni.so" \
    "$output_dir/armeabi-v7a/libbhttpjni.so" \
    "$output_dir/x86_64/libbhttpjni.so" \
    "$output_dir/x86/libbhttpjni.so"
