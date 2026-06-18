import sys
from pathlib import Path

# Add src/ to sys.path so `import tag` resolves when tests load controller.py
# via spec_from_file_location (which doesn't go through the installed package).
sys.path.insert(0, str(Path(__file__).parent.parent / "src"))

