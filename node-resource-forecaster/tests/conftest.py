import os
import sys

# Make the component root importable so tests can `import models` and `import training`.
COMPONENT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if COMPONENT_ROOT not in sys.path:
    sys.path.insert(0, COMPONENT_ROOT)
