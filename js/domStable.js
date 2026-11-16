// waitDOMStable returns a promise that resolves when the DOM does not mutate
// during quietMs milliseconds. It resolves with an object that tells whether
// the DOM turned stable before timeoutMs milliseconds passed, and how many
// mutations it saw. Mutations of attributes count only when attributes is
// true, so that an animation that changes a style or a class does not stop
// the wait. A timeoutMs of 0 disables the timeout.
function waitDOMStable(quietMs, timeoutMs, attributes) {
    return new Promise(resolve => {
        const start = performance.now();
        let last = start;
        let mutations = 0;
        const observer = new MutationObserver(list => {
            mutations += list.length;
            last = performance.now();
        });
        observer.observe(document, {
            subtree: true,
            childList: true,
            characterData: true,
            attributes: attributes,
        });
        const step = Math.min(50, Math.max(quietMs / 4, 5));
        const tick = () => {
            const now = performance.now();
            if (now - last >= quietMs) {
                observer.disconnect();
                resolve({ stable: true, mutations: mutations });
            } else if (timeoutMs > 0 && now - start >= timeoutMs) {
                observer.disconnect();
                resolve({ stable: false, mutations: mutations });
            } else {
                setTimeout(tick, step);
            }
        };
        tick();
    });
}
