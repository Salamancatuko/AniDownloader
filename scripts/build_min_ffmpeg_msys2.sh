#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

if [[ $# -ge 1 && -n "${1}" ]]; then
    RAW_ROOT="$1"
    if command -v cygpath >/dev/null 2>&1; then
        RAW_ROOT="$(cygpath -u "${RAW_ROOT}")"
    fi
    PROJECT_ROOT="$(cd "${RAW_ROOT}" && pwd)"
else
    PROJECT_ROOT="${DEFAULT_PROJECT_ROOT}"
fi

FFMPEG_VER="${FFMPEG_VER:-7.1.1}"
BUILD_DIR="${BUILD_DIR:-${HOME}/ffmpeg-slim-build}"
SRC_ARCHIVE="ffmpeg-${FFMPEG_VER}.tar.xz"
SRC_URL="https://ffmpeg.org/releases/${SRC_ARCHIVE}"
SRC_DIR="${BUILD_DIR}/ffmpeg-${FFMPEG_VER}"
TARGET_DIR="${PROJECT_ROOT}/bin_embed"
TARGET_GZ="${TARGET_DIR}/ffmpeg.exe.gz"

echo "==> Project root: ${PROJECT_ROOT}"
echo "==> Build dir:    ${BUILD_DIR}"
echo "==> FFmpeg ver:   ${FFMPEG_VER}"

if [[ -n "${MSYSTEM:-}" && "${MSYSTEM}" != "MINGW64" ]]; then
    echo "ERROR: Detected MSYSTEM=${MSYSTEM}. Please run this script inside the 'MSYS2 MINGW64' shell." >&2
    exit 1
fi

required_tools=(gcc nasm objdump strip gzip curl tar make)
for tool in "${required_tools[@]}"; do
    if ! command -v "${tool}" >/dev/null 2>&1; then
        echo "ERROR: Missing required tool '${tool}'. Install MSYS2 MINGW64 toolchain packages first:" >&2
        echo "  pacman -S --needed base-devel mingw-w64-x86_64-toolchain mingw-w64-x86_64-nasm mingw-w64-x86_64-pkgconf diffutils make gzip tar xz curl" >&2
        exit 1
    fi
done

mkdir -p "${BUILD_DIR}"
cd "${BUILD_DIR}"

if [[ ! -s "${SRC_ARCHIVE}" ]]; then
    echo "==> Downloading ${SRC_URL}..."
    curl -fL --retry 3 -o "${SRC_ARCHIVE}.part" "${SRC_URL}"
    mv -f "${SRC_ARCHIVE}.part" "${SRC_ARCHIVE}"
else
    echo "==> Reusing existing archive ${BUILD_DIR}/${SRC_ARCHIVE}"
fi

echo "==> Extracting ${SRC_ARCHIVE}..."
rm -rf "${SRC_DIR}"
tar -xf "${SRC_ARCHIVE}"
cd "${SRC_DIR}"

echo "==> Configuring minimal static LGPL FFmpeg..."
./configure \
    --target-os=mingw32 \
    --arch=x86_64 \
    --enable-static \
    --disable-shared \
    --enable-small \
    --disable-debug \
    --disable-doc \
    --disable-htmlpages \
    --disable-manpages \
    --disable-podpages \
    --disable-txtpages \
    --disable-ffplay \
    --disable-ffprobe \
    --disable-avdevice \
    --disable-postproc \
    --disable-autodetect \
    --disable-pthreads \
    --enable-w32threads \
    --disable-everything \
    --enable-schannel \
    --enable-decoder=aac,aac_latm,h264 \
    --enable-protocol=file,pipe,http,https,httpproxy,tls,tcp,crypto,data \
    --enable-demuxer=hls,mpegts,mov,matroska,concat,aac,mp3,ogg,flv \
    --enable-muxer=mp4,ipod,matroska,webm,mpegts,null \
    --enable-parser=h264,hevc,av1,vp9,vp8,aac,aac_latm,opus,vorbis,mpegaudio \
    --enable-bsf=aac_adtstoasc,h264_mp4toannexb,hevc_mp4toannexb,extract_extradata,vp9_superframe,av1_metadata \
    --extra-cflags="-Os -std=gnu17 -ffunction-sections -fdata-sections" \
    --extra-ldflags="-static -static-libgcc -Wl,--gc-sections -s"

JOBS="$(nproc 2>/dev/null || echo 4)"
echo "==> Building with ${JOBS} jobs..."
make -j"${JOBS}"
strip --strip-all ffmpeg.exe

echo "==> Checking DLL dependencies..."
dll_lines="$(objdump -p ffmpeg.exe | grep "DLL Name:" || true)"
if [[ -z "${dll_lines}" ]]; then
    echo "ERROR: objdump -p ffmpeg.exe reported no DLL imports; unexpected PE output." >&2
    exit 1
fi
printf '%s\n' "${dll_lines}"

while IFS= read -r line; do
    dll="${line#*DLL Name:}"
    dll="$(printf '%s' "${dll}" | tr -d '[:space:]\r')"
    dll_lower="$(printf '%s' "${dll}" | tr '[:upper:]' '[:lower:]')"
    case "${dll_lower}" in
        kernel32.dll|msvcrt.dll|ws2_32.dll|secur32.dll|bcrypt.dll|shell32.dll|advapi32.dll|user32.dll|ole32.dll|crypt32.dll|ncrypt.dll|shlwapi.dll|ucrtbase.dll|api-ms-win-*.dll)
            ;;
        lib*.dll|zlib*.dll|msys-*.dll|cyg*.dll|*)
            echo "ERROR: Forbidden or unexpected non-system DLL dependency detected: ${dll}" >&2
            exit 1
            ;;
    esac
done <<< "${dll_lines}"

echo "==> Checking version output..."
ver_out="$(./ffmpeg.exe -version | head -n 3)"
printf '%s\n' "${ver_out}"
if ! printf '%s\n' "${ver_out}" | grep -Eq '^ffmpeg version [0-9]+\.[0-9]+'; then
    echo "ERROR: ffmpeg.exe -version format is incompatible with embed_bin.go ffmpegVerRe." >&2
    exit 1
fi

echo "==> Verifying required protocols, demuxers, muxers, and bitstream filters..."
protocols_out="$(./ffmpeg.exe -hide_banner -protocols)"
for p in file http https tls crypto; do
    if ! printf '%s\n' "${protocols_out}" | tr -d '\r' | grep -Eq "^[[:space:]]*${p}[[:space:]]*$"; then
        echo "ERROR: Missing required protocol '${p}'" >&2
        exit 1
    fi
done

demuxers_out="$(./ffmpeg.exe -hide_banner -demuxers)"
for d in hls mpegts mov matroska concat; do
    if ! printf '%s\n' "${demuxers_out}" | tr -d '\r' | awk '/^[[:space:]]*--/{flag=1; next} flag && NF>=2 {print $2}' | tr ',' '\n' | grep -Fxq "${d}"; then
        echo "ERROR: Missing required demuxer '${d}'" >&2
        exit 1
    fi
done

muxers_out="$(./ffmpeg.exe -hide_banner -muxers)"
for m in mp4 matroska mpegts; do
    if ! printf '%s\n' "${muxers_out}" | tr -d '\r' | awk '/^[[:space:]]*--/{flag=1; next} flag && NF>=2 {print $2}' | tr ',' '\n' | grep -Fxq "${m}"; then
        echo "ERROR: Missing required muxer '${m}'" >&2
        exit 1
    fi
done

bsfs_out="$(./ffmpeg.exe -hide_banner -bsfs)"
if ! printf '%s\n' "${bsfs_out}" | tr -d '\r' | grep -Eq "^[[:space:]]*aac_adtstoasc[[:space:]]*$"; then
    echo "ERROR: Missing required bitstream filter 'aac_adtstoasc'" >&2
    exit 1
fi

decoders_out="$(./ffmpeg.exe -hide_banner -decoders)"
for dec in h264 aac aac_latm; do
    if ! printf '%s\n' "${decoders_out}" | tr -d '\r' | awk '/^[[:space:]]*--/{flag=1; next} flag && NF>=2 {print $2}' | tr ',' '\n' | grep -Fxq "${dec}"; then
        echo "ERROR: Missing required decoder '${dec}'" >&2
        exit 1
    fi
done

echo "==> Compressing ffmpeg.exe -> ffmpeg.exe.gz..."
gzip -9 -c ffmpeg.exe > ffmpeg.exe.gz

old_bytes=0
if [[ -f "${TARGET_GZ}" ]]; then
    old_bytes="$(wc -c < "${TARGET_GZ}" | tr -d '[:space:]')"
fi

mkdir -p "${TARGET_DIR}"
tmp_target="${TARGET_GZ}.tmp.$$"
cp -f ffmpeg.exe.gz "${tmp_target}"
mv -f "${tmp_target}" "${TARGET_GZ}"

exe_bytes="$(wc -c < ffmpeg.exe | tr -d '[:space:]')"
new_bytes="$(wc -c < "${TARGET_GZ}" | tr -d '[:space:]')"

format_mib() {
    awk -v b="$1" 'BEGIN { printf "%.2f", b / 1048576.0 }'
}

echo "==> Installed ${TARGET_GZ}"
echo "    ffmpeg.exe (raw):   ${exe_bytes} bytes ($(format_mib "${exe_bytes}") MiB)"
if [[ "${old_bytes}" -gt 0 ]]; then
    echo "    ffmpeg.exe.gz old:  ${old_bytes} bytes ($(format_mib "${old_bytes}") MiB)"
fi
echo "    ffmpeg.exe.gz new:  ${new_bytes} bytes ($(format_mib "${new_bytes}") MiB)"
