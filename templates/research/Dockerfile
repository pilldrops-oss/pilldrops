# Deep Research — use-case template over the code-interpreter image.
# In production the rootfs is a HARDLINK to code-interpreter's on the node
# (zero extra disk, instant), and the docker runtime maps this id to the
# code-interpreter enriched image — so this Dockerfile exists for provenance
# and for standalone bakes: build the code-interpreter template first
# (scripts/build-template.sh code-interpreter tags sandbox-code-interpreter).
FROM sandbox-code-interpreter
# Research-specific additions go here when the use case outgrows the shared
# image (e.g. pandoc for report export, playwright for browsing).
WORKDIR /root
