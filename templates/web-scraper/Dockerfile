# Web Scraper — use-case template over the code-interpreter image.
# Prod: rootfs hardlinked to code-interpreter on the node; docker maps to the
# code-interpreter enriched image (bs4 installed via SetupSuggestion).
FROM sandbox-code-interpreter
RUN pip install --no-cache-dir beautifulsoup4
WORKDIR /root
