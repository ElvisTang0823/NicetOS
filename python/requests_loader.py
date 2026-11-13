import importlib
import importlib.util
import os
import sys


def load_requests(root):
    site_packages = os.path.join(root, "python_runtime", "Lib", "site-packages")
    package_dir = os.path.join(site_packages, "requests")
    package_init = os.path.join(package_dir, "__init__.py")

    if not os.path.isdir(site_packages):
        requests = importlib.import_module("requests")
    else:
        if not os.path.isfile(package_init):
            raise ImportError(f"bundled requests package is missing: {package_init}")

        sys.path.insert(0, site_packages)
        for name in tuple(sys.modules):
            if name == "requests" or name.startswith("requests."):
                del sys.modules[name]

        spec = importlib.util.spec_from_file_location(
            "requests",
            package_init,
            submodule_search_locations=[package_dir],
        )
        if spec is None or spec.loader is None:
            raise ImportError(f"cannot load bundled requests package: {package_init}")
        requests = importlib.util.module_from_spec(spec)
        sys.modules["requests"] = requests
        try:
            spec.loader.exec_module(requests)
        except Exception:
            for name in tuple(sys.modules):
                if name == "requests" or name.startswith("requests."):
                    del sys.modules[name]
            raise

    missing = [name for name in ("get", "post", "exceptions") if not hasattr(requests, name)]
    if missing:
        module_path = getattr(requests, "__file__", None)
        raise ImportError(f"requests module at {module_path} is missing: {', '.join(missing)}")
    return requests
