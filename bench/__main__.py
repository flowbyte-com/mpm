"""Entry point for `python -m bench`.

This module is intentionally thin. The actual work lives in
`bench.harness`. Running this from a non-`bench/` directory will
work because Python's `-m` flag handles the module path; tests
discover the module from the package root.
"""

import sys

from bench.harness import main


if __name__ == "__main__":
    sys.exit(main())
