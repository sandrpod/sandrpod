# Copyright 2024 SandrPod
# Setup

from pathlib import Path

from setuptools import setup

_readme = Path(__file__).parent / "sandrpod_cli" / "README.md"
long_description = _readme.read_text(encoding="utf-8") if _readme.exists() else ""

setup(
    name="sandrpod-cli",
    version="0.2.2",
    description=(
        "Command-line client for SandrPod — self-hosted, multi-cloud sandbox "
        "infrastructure for AI agents"
    ),
    long_description=long_description,
    long_description_content_type="text/markdown",
    license="MIT",
    python_requires=">=3.8",
    packages=["sandrpod_cli"],
    package_data={"sandrpod_cli": ["py.typed", "README.md"]},
    include_package_data=True,
    install_requires=[
        "requests>=2.28.0",
        "click>=8.0.0",
        "pyyaml>=6.0",
    ],
    extras_require={
        "shell": ["websocket-client>=1.0.0"],
    },
    entry_points={
        "console_scripts": [
            "sandrpod-cli=sandrpod_cli.main:cli",
        ],
    },
    project_urls={
        "Homepage": "https://github.com/sandrpod/sandrpod",
        "Repository": "https://github.com/sandrpod/sandrpod",
    },
    classifiers=[
        "Intended Audience :: Developers",
        "License :: OSI Approved :: MIT License",
        "Programming Language :: Python :: 3",
        "Topic :: Scientific/Engineering :: Artificial Intelligence",
    ],
)
