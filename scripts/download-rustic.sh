#!/bin/bash
# download-rustic.sh - Downloads rustic binaries for embedding

set -euo pipefail

RUSTIC_VERSION="v0.10.0"
BINARIES_DIR="$(dirname "$0")/../src/internal/rustic/binaries"
GITHUB_BASE_URL="https://github.com/rustic-rs/rustic/releases/download/${RUSTIC_VERSION}"

# Create binaries directory
mkdir -p "${BINARIES_DIR}"

# Platform mappings: Go GOOS/GOARCH -> rustic release name (Linux only)
declare -A PLATFORMS=(
    ["linux/amd64"]="rustic-${RUSTIC_VERSION}-x86_64-unknown-linux-gnu"
    ["linux/arm64"]="rustic-${RUSTIC_VERSION}-aarch64-unknown-linux-gnu"
)

echo "Downloading rustic binaries for embedding..."

for platform in "${!PLATFORMS[@]}"; do
    rustic_name="${PLATFORMS[$platform]}"
    goos="${platform%/*}"
    goarch="${platform#*/}"

    output_file="${BINARIES_DIR}/rustic-${goos}-${goarch}"
    tarball_name="${rustic_name}.tar.gz"
    download_url="${GITHUB_BASE_URL}/${tarball_name}"
    temp_tarball="/tmp/${tarball_name}"

    echo "Downloading ${platform}: ${download_url}"

    # Download tarball with retries and proper error handling
    if curl -L --fail --retry 3 --retry-delay 2 -o "${temp_tarball}" "${download_url}"; then
        # Extract the rustic binary from tarball
        if tar -xzf "${temp_tarball}" -C /tmp/ rustic; then
            mv "/tmp/rustic" "${output_file}"
            rm -f "${temp_tarball}"

            # Verify extraction
            if [[ ! -s "${output_file}" ]]; then
                echo "Error: Extracted file ${output_file} is empty"
                rm -f "${output_file}"
                exit 1
            fi

            # Set executable permissions
            chmod +x "${output_file}"

            # Get file size for verification
            size=$(stat -c%s "${output_file}" 2>/dev/null || stat -f%z "${output_file}" 2>/dev/null || echo "unknown")
            echo "Downloaded and extracted ${output_file} (${size} bytes)"
        else
            echo "Error: Failed to extract rustic binary from ${temp_tarball}"
            rm -f "${temp_tarball}"
            exit 1
        fi
    else
        echo "Error: Failed to download ${download_url}"
        exit 1
    fi
done

echo "All rustic binaries downloaded successfully to ${BINARIES_DIR}"
echo "Verifying checksums (if available)..."

# Try to download and verify checksums if available
checksum_url="${GITHUB_BASE_URL}/rustic-${RUSTIC_VERSION}-checksums.txt"
if curl -L --fail --silent -o "${BINARIES_DIR}/checksums.txt" "${checksum_url}" 2>/dev/null; then
    echo "Checksums downloaded, verifying..."
    cd "${BINARIES_DIR}"

    # Verify each downloaded binary
    for platform in "${!PLATFORMS[@]}"; do
        rustic_name="${PLATFORMS[$platform]}"
        goos="${platform%/*}"
        goarch="${platform#*/}"

        if [[ "$goos" == "windows" ]]; then
            ext=".exe"
        else
            ext=""
        fi

        output_file="rustic-${goos}-${goarch}${ext}"

        if [[ -f "$output_file" ]] && [[ -f "checksums.txt" ]]; then
            # Look for checksum in the checksums file
            if grep -q "${rustic_name}" checksums.txt; then
                echo "Verifying checksum for ${output_file}..."
                # Extract expected checksum for this binary
                expected_checksum=$(grep "${rustic_name}" checksums.txt | awk '{print $1}')

                # Calculate actual checksum
                if command -v sha256sum >/dev/null; then
                    actual_checksum=$(sha256sum "${output_file}" | awk '{print $1}')
                elif command -v shasum >/dev/null; then
                    actual_checksum=$(shasum -a 256 "${output_file}" | awk '{print $1}')
                else
                    echo "Warning: No SHA256 utility found, skipping checksum verification"
                    continue
                fi

                if [[ "$expected_checksum" == "$actual_checksum" ]]; then
                    echo "✓ Checksum verified for ${output_file}"
                else
                    echo "✗ Checksum mismatch for ${output_file}"
                    echo "  Expected: $expected_checksum"
                    echo "  Actual:   $actual_checksum"
                    exit 1
                fi
            else
                echo "Warning: No checksum found for ${rustic_name}"
            fi
        fi
    done

    rm -f checksums.txt
    cd - >/dev/null
else
    echo "Warning: Could not download checksums file, skipping verification"
fi

echo "Rustic binary download and verification completed successfully!"