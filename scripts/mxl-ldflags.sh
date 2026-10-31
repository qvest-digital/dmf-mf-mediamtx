#!/bin/sh
# Prints the cgo link flags of the MXL source: libmxl, libmxl-monitoring
# with its OTLP exporter under /opt/mxl-monitoring, and opentelemetry-cpp
# under /opt/otel, the last two static. Dockerfile.mxl and CI both link
# through this, so the image and the tests link the same way.
#
# cgo refuses a compiler flag in opentelemetry-cpp's pkg-config files, which
# also omit its curl HTTP client, so the libraries come from here rather than
# from a #cgo pkg-config line. They are grouped with the libmxl-monitoring
# archives, which the Go binding names in its own flags too: cgo places those
# after this group in a binary with many cgo packages, where a group no
# longer resolves them.
set -eu

otel=/opt/otel/x64-linux/lib
libs=$(PKG_CONFIG_PATH="$otel/pkgconfig" pkg-config --static --libs \
    opentelemetry_exporter_otlp_http_metric opentelemetry_exporter_otlp_http_log libcurl)
echo "-L/opt/libmxl/lib -lmxl -L/opt/mxl-monitoring/lib" \
    "-Wl,--start-group -lmxl-monitoring-otlp -lmxl-monitoring-prometheus -lmxl-monitoring" \
    "-lopentelemetry_http_client_curl $libs -lstdc++ -Wl,--end-group"
