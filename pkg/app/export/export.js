function toggle(header) {
    const content = header.nextElementSibling;
    const chevronRight = header.querySelector('.chevron-right');
    const chevronDown = header.querySelector('.chevron-down');
    const isHidden = content.style.display === 'none';
    content.style.display = isHidden ? 'block' : 'none';
    if (chevronRight) chevronRight.style.display = isHidden ? 'none' : 'block';
    if (chevronDown) chevronDown.style.display = isHidden ? 'block' : 'none';
}

document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
        document.querySelectorAll('.collapsible-content').forEach(el => el.style.display = 'none');
        document.querySelectorAll('.chevron-right').forEach(icon => icon.style.display = 'block');
        document.querySelectorAll('.chevron-down').forEach(icon => icon.style.display = 'none');
    }
});

// Prevent browser scroll restoration so we always land at the bottom.
if ('scrollRestoration' in history) {
    history.scrollRestoration = 'manual';
}

function scrollToBottom() {
    window.scrollTo(0, document.body.scrollHeight);
}

window.addEventListener('DOMContentLoaded', scrollToBottom);
window.addEventListener('load', () => setTimeout(scrollToBottom, 50));

// Detect live-reload content changes and scroll to bottom.
new MutationObserver(scrollToBottom).observe(document.documentElement, {
    childList: true, subtree: true
});
