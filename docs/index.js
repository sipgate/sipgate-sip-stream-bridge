
const autoDetectLanguage = () => {
    const languages = navigator.languages || [navigator.language || ''];
    const supportsGerman = languages.some(language => language.toLowerCase().startsWith('de'));
    const path = window.location.pathname.replace(/\/index\.html$/, '/');
    const isDefaultGermanPage = !path.endsWith('/en/') && !path.includes('/en/');

    if (isDefaultGermanPage && !supportsGerman && !sessionStorage.getItem('languageAutoDetected')) {
        sessionStorage.setItem('languageAutoDetected', 'true');
        window.location.href = 'en/';
    }
};

autoDetectLanguage();

const hamburger = document.getElementById('hamburger');
const mobileMenu = document.getElementById('mobile-menu');
hamburger.addEventListener('click', () => {
    hamburger.classList.toggle('open');
    mobileMenu.classList.toggle('open');
});
mobileMenu.querySelectorAll('a').forEach(a => {
    a.addEventListener('click', () => {
        hamburger.classList.remove('open');
        mobileMenu.classList.remove('open');
    });
});

const revealEls = document.querySelectorAll('.reveal');
const observer = new IntersectionObserver((entries) => {
    entries.forEach(e => {
        if (e.isIntersecting) { e.target.classList.add('visible'); observer.unobserve(e.target); }
    });
}, { threshold: 0.1, rootMargin: '0px 0px -40px 0px' });
revealEls.forEach(el => observer.observe(el));

document.querySelectorAll('.hero .reveal').forEach((el, i) => {
    setTimeout(() => el.classList.add('visible'), i * 100);
});

function toggleFaq(btn) {
    const item = btn.closest('.faq-item');
    const answer = item.querySelector('.faq-answer');
    const isOpen = item.classList.contains('open');
    document.querySelectorAll('.faq-item').forEach(i => {
        i.classList.remove('open');
        i.querySelector('.faq-answer').style.maxHeight = null;
    });
    if (!isOpen) {
        item.classList.add('open');
        answer.style.maxHeight = answer.scrollHeight + 'px';
    }
}

function copyCode(id, btn) {
    const el = document.getElementById(id);
    const text = el.innerText.replace(/^(Kopieren|Copy)\n?/, '').trim();
    navigator.clipboard.writeText(text).then(() => {
        const orig = btn.textContent;
        btn.textContent = '✓ Kopiert';
        btn.classList.add('copied');
        setTimeout(() => { btn.textContent = orig; btn.classList.remove('copied'); }, 2000);
    }).catch(() => {
        const range = document.createRange();
        range.selectNodeContents(el);
        window.getSelection().removeAllRanges();
        window.getSelection().addRange(range);
    });
}
