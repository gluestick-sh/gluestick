//! Golden tests against real Scoop bucket manifests.
//!
//! Fixtures in `tests/data/` are verbatim copies from the local `main`/`java`
//! buckets (`~/.glue/buckets`), covering arch blocks, bin/shortcut variants,
//! hooks, env, and autoupdate passthrough.

use gluestick_core::manifest::{parse_str, ARCH_32BIT, ARCH_64BIT, ARCH_ARM64};

const SEVENZIP: &str = include_str!("data/7zip.json");
const GIT: &str = include_str!("data/git.json");
const VIM: &str = include_str!("data/vim.json");
const TEMURIN_JRE: &str = include_str!("data/temurin-jre.json");

#[test]
fn golden_7zip() {
    let m = parse_str(SEVENZIP).unwrap();
    assert_eq!(m.version, "26.04");
    assert!(m.get_license().contains("BSD-2-Clause"));
    // Notes: 5 entries incl. one blank separator → drops empty strings → 4.
    assert_eq!(m.get_notes().len(), 4);

    // Arch URLs via explicit overrides (host-independent).
    assert_eq!(
        m.get_urls_for_install(ARCH_64BIT),
        vec!["https://github.com/ip7z/7zip/releases/download/26.04/7z2604-x64.msi"]
    );
    assert!(m.get_urls_for_install(ARCH_ARM64)[0].ends_with("7z2604-arm64.exe"));
    assert_eq!(m.get_extract_dir_for_install(ARCH_64BIT), "Files\\7-Zip");
    let hashes = m.get_hashes_for_install(ARCH_64BIT);
    assert_eq!(hashes.len(), 1);
    assert_eq!(hashes[0].len(), 64);

    // Root fields.
    assert_eq!(m.binaries(), vec!["7z.exe", "7zG.exe", "7zFM.exe"]);
    let shortcuts = m.shortcut_entries_for_install("64bit");
    assert_eq!(shortcuts.len(), 2);
    assert_eq!(shortcuts[0].target, "7zFM.exe");
    assert_eq!(shortcuts[0].label, "7-Zip\\7-Zip File Manager");
    assert_eq!(
        m.persist.as_ref().and_then(|v| v.as_array()).map(Vec::len),
        Some(2)
    );

    // Hooks: root post_install (6 entries); arm64-only pre_install.
    assert_eq!(m.post_install_hooks_for_install("").len(), 6);
    assert_eq!(m.pre_install_hooks_for_install(ARCH_ARM64).len(), 6);
    assert!(m.pre_install_hooks_for_install(ARCH_64BIT).is_empty());
    // Root uninstaller.script (string form).
    assert_eq!(m.uninstaller_script_hooks_for_install("").len(), 1);
    assert!(m.autoupdate.is_some());
    assert!(!m.is_deprecated_marked());
    assert_eq!(
        m.available_architectures(),
        vec![ARCH_ARM64, ARCH_64BIT, ARCH_32BIT]
    );
}

#[test]
fn golden_git() {
    let m = parse_str(GIT).unwrap();
    assert_eq!(m.version, "2.56.0.2");

    // Raw manifest URLs keep the Scoop #/dl.7z fragment (downloader parses it).
    let urls = m.get_urls_for_install(ARCH_64BIT);
    assert_eq!(urls.len(), 1);
    assert!(urls[0].contains("#/dl.7z"));
    assert!(urls[0].contains("PortableGit-2.56.0.2-64-bit"));
    assert_eq!(m.get_hashes_for_install(ARCH_64BIT)[0].len(), 64);

    // env_set lives at root; host block has none → root survives on any host.
    assert_eq!(
        m.env_set_for_install("")
            .get("GIT_INSTALL_ROOT")
            .map(String::as_str),
        Some("$dir")
    );

    assert_eq!(m.binaries().len(), 11);
    assert_eq!(m.binaries()[0], "bin\\sh.exe");

    let shortcuts = m.shortcut_entries_for_install("");
    assert_eq!(shortcuts.len(), 3);
    assert_eq!(shortcuts[0].args, "--cd-to-home");
    assert_eq!(shortcuts[2].target, "cmd\\git-gui.exe");
    assert_eq!(shortcuts[2].args, "");

    // 8 entries incl. two blank separators → drops empty strings → 6.
    let notes = m.get_notes();
    assert_eq!(notes.len(), 6);
    assert!(notes[0].starts_with("To register file associations"));

    let pre = m.pre_install_hooks_for_install("");
    assert!(pre.len() >= 10, "pre_install len = {}", pre.len());
    assert!(pre[0].starts_with("# Hardlinks"));
    assert_eq!(pre.last().map(String::as_str), Some("}"));

    let pre_un = m.pre_uninstall_hooks_for_install("");
    assert!(pre_un.len() >= 5);
    assert!(pre_un[0].starts_with("# Hardlinks"));
    assert_eq!(m.uninstaller_script_hooks_for_install("").len(), 4);
}

#[test]
fn golden_vim() {
    let m = parse_str(VIM).unwrap();
    assert_eq!(m.version, "9.2.1186");
    assert_eq!(m.get_license(), "Vim");
    assert_eq!(
        m.get_notes(),
        vec!["Add gVim as a context menu option by running: \"$dir\\install-context.reg\""]
    );
    // Root extract_dir wins on every host.
    assert_eq!(m.get_extract_dir(), "vim/vim92");

    let suggest = m.suggestions();
    assert_eq!(suggest.len(), 1);
    assert_eq!(
        (suggest[0].label.as_str(), suggest[0].ref_.as_str()),
        ("vimtutor", "vimtutor")
    );

    // bin mixes plain strings and [path, alias, args] pairs (3rd element dropped).
    let bins = m.binaries();
    assert_eq!(bins.len(), 15);
    assert_eq!(bins[0], "vim.exe");
    assert_eq!(bins[1], "[vim.exe,vi]");
    assert_eq!(bins[2], "[vim.exe,ex]"); // "-e" args are not part of the pattern
    assert_eq!(bins[7], "gvim.exe");
    assert_eq!(bins.last().map(String::as_str), Some("xxd.exe"));

    let shortcuts = m.shortcut_entries_for_install("");
    assert_eq!(shortcuts.len(), 1);
    assert_eq!(shortcuts[0].target, "gvim.exe");
    assert_eq!(shortcuts[0].label, "gVim");

    assert!(m.get_urls_for_install(ARCH_32BIT)[0].contains("x86_signed.zip"));
    assert_eq!(
        m.available_architectures(),
        vec![ARCH_ARM64, ARCH_64BIT, ARCH_32BIT]
    );
}

#[test]
fn golden_temurin_jre() {
    let m = parse_str(TEMURIN_JRE).unwrap();
    assert_eq!(m.version, "27.0.0-35");
    assert_eq!(m.homepage, "https://adoptium.net");
    // Root env_add_path / env_set / extract_dir.
    assert_eq!(m.env_add_paths(), vec!["bin"]);
    assert_eq!(
        m.env_set_for_install("")
            .get("JAVA_HOME")
            .map(String::as_str),
        Some("$dir")
    );
    assert_eq!(m.get_extract_dir(), "jdk-27+35-jre");
    // Only a 64bit block exists.
    assert_eq!(m.available_architectures(), vec![ARCH_64BIT]);
    let urls = m.get_urls_for_install(ARCH_64BIT);
    assert_eq!(urls.len(), 1);
    assert!(urls[0].contains("OpenJDK27U-jre_x64_windows_hotspot"));
    assert_eq!(m.get_hashes_for_install(ARCH_64BIT)[0].len(), 64);
}
