"""MPM tools registered with Hermes Agent's tool registry.

Auto-discovered by discover_builtin_tools() because it lives in the tools/
directory. Each module calls registry.register() at import time.

Set MPM_WORKSPACE and MPM_BINARY env vars before starting Hermes Agent:
    export MPM_WORKSPACE=/path/to/workspace
    export MPM_BINARY=/path/to/mpm   # optional, defaults to "mpm" in PATH
"""

from . import epistemology_tools  # noqa: F401 — registers epistemology tools
from . import lesson_tools       # noqa: F401 — registers lesson tools
from . import memory_tools        # noqa: F401 — registers memory tools
from . import recall_tools        # noqa: F401 — registers recall tools
from . import reference_tools     # noqa: F401 — registers reference tools
from . import session_tools       # noqa: F401 — registers session tools
from . import topic_tools         # noqa: F401 — registers topic tools
from . import evidence_tools      # noqa: F401 — registers evidence and confidence tools

__all__ = [
    "epistemology_tools",
    "lesson_tools",
    "memory_tools",
    "recall_tools",
    "reference_tools",
    "session_tools",
    "topic_tools",
    "evidence_tools",
]