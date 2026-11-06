import argparse
import hashlib
import io
import json
import tarfile
from pathlib import Path


VERSION = "ollama-v0.40.13"
TAG = "nicetos-ollama-v0.40.13"
MAX_PART_SIZE = 1_800_000_000


class SplitWriter:
    def __init__(self, output_dir: Path):
        self.output_dir = output_dir
        self.total_size = 0
        self.position = 0
        self.archive_hash = hashlib.sha256()
        self.parts = []
        self.current_file = None
        self.current_hash = None
        self.current_size = 0

    def tell(self):
        return self.position

    def flush(self):
        if self.current_file:
            self.current_file.flush()

    def close_part(self):
        if not self.current_file:
            return
        self.current_file.close()
        self.parts.append({
            "name": self.current_path.name,
            "size": self.current_size,
            "sha256": self.current_hash.hexdigest(),
        })
        self.current_file = None
        self.current_hash = None
        self.current_size = 0

    def open_part(self):
        name = f"ollama-bundle.tar.gz.part{len(self.parts) + 1:03d}"
        self.current_path = self.output_dir / name
        self.current_file = self.current_path.open("wb")
        self.current_hash = hashlib.sha256()

    def write(self, data):
        view = memoryview(data)
        written = 0
        while written < len(view):
            if not self.current_file:
                self.open_part()
            available = MAX_PART_SIZE - self.current_size
            count = min(available, len(view) - written)
            chunk = view[written:written + count]
            self.current_file.write(chunk)
            self.current_hash.update(chunk)
            self.archive_hash.update(chunk)
            self.current_size += count
            self.total_size += count
            self.position += count
            written += count
            if self.current_size == MAX_PART_SIZE:
                self.close_part()
        return written

    def finish(self):
        self.close_part()


def bundle_files(root: Path):
    ollama = root / "ollama"
    bin_dir = ollama / "bin"
    modelfile = ollama / "models" / "Modelfile"
    model = ollama / "models" / "gemma4_child_safety_q4.gguf"
    for path in (bin_dir, modelfile, model):
        if not path.exists():
            raise FileNotFoundError(f"required Ollama bundle input is missing: {path}")

    files = [path for path in bin_dir.rglob("*") if path.is_file()]
    files.extend((modelfile, model))
    expanded_size = sum(path.stat().st_size for path in files) + len((VERSION + "\n").encode())
    return bin_dir, modelfile, model, expanded_size


def main():
    parser = argparse.ArgumentParser(
        description="Build sub-2-GiB GitHub Release assets for the bundled Ollama runtime and model."
    )
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()

    root = args.root.resolve()
    output_dir = args.output.resolve() if args.output else root / "release-assets" / TAG
    if output_dir.exists() and any(output_dir.iterdir()):
        raise FileExistsError(f"release asset directory is not empty: {output_dir}")
    output_dir.mkdir(parents=True, exist_ok=True)

    bin_dir, modelfile, model, expanded_size = bundle_files(root)
    writer = SplitWriter(output_dir)
    marker = (VERSION + "\n").encode()
    with tarfile.open(fileobj=writer, mode="w|gz", compresslevel=1) as archive:
        archive.add(bin_dir, arcname="ollama/bin", recursive=True)
        archive.add(modelfile, arcname="ollama/models/Modelfile")
        archive.add(model, arcname="ollama/models/gemma4_child_safety_q4.gguf")
        marker_info = tarfile.TarInfo("ollama/.bundle-version")
        marker_info.size = len(marker)
        archive.addfile(marker_info, io.BytesIO(marker))
    writer.finish()

    metadata = {
        "version": VERSION,
        "release_tag": TAG,
        "archive_name": "ollama-bundle.tar.gz",
        "archive_size": writer.total_size,
        "archive_sha256": writer.archive_hash.hexdigest(),
        "expanded_size": expanded_size,
        "parts": writer.parts,
    }
    manifest_path = output_dir / "ollama-bundle-manifest.json"
    manifest_path.write_text(json.dumps(metadata, indent=2) + "\n", encoding="utf-8")
    print(f"Release tag: {TAG}")
    print(f"Expanded payload: {expanded_size:,} bytes")
    print(f"Compressed archive: {writer.total_size:,} bytes")
    for part in writer.parts:
        print(f"{part['name']}: {part['size']:,} bytes SHA256={part['sha256']}")
    print(f"Manifest: {manifest_path}")
    print("Upload the manifest and every part as assets of the public GitHub Release.")


if __name__ == "__main__":
    main()
