(function () {
  var deck = document.getElementById('deck');
  var toggle = document.getElementById('motion-toggle');
  if (!deck || !toggle || !('IntersectionObserver' in window)) return;

  var reduced = matchMedia('(prefers-reduced-motion: reduce)');
  var paused = false;
  var artworks = Array.from(document.querySelectorAll('.art-motion')).map(function (figure) {
    return { figure: figure, video: figure.querySelector('video'), visible: false, failed: false };
  });

  function shouldPlay(art) {
    return art.visible && !art.failed && !paused && !reduced.matches && !document.hidden;
  }

  function update() {
    toggle.hidden = reduced.matches || !artworks.some(function (art) { return art.visible && !art.failed; });
    toggle.textContent = paused ? 'Play motion' : 'Pause motion';
    artworks.forEach(function (art) {
      if (!shouldPlay(art)) {
        art.video.pause();
        art.figure.classList.remove('is-playing');
        return;
      }
      if (!art.video.hasAttribute('src')) art.video.src = art.video.dataset.src;
      if (!art.video.paused) return;
      art.video.play().catch(function (error) {
        if (error.name === 'AbortError') return;
        // A blocked or unavailable video leaves the original engraving visible.
        art.failed = true;
        update();
      });
    });
  }

  artworks.forEach(function (art) {
    art.video.muted = true;
    art.video.addEventListener('playing', function () {
      if (shouldPlay(art)) art.figure.classList.add('is-playing');
      else update();
    });
    art.video.addEventListener('error', function () {
      art.failed = true;
      update();
    });
  });

  var observer = new IntersectionObserver(function (entries) {
    entries.forEach(function (entry) {
      var art = artworks.find(function (item) { return item.figure === entry.target; });
      art.visible = entry.isIntersecting && entry.intersectionRatio >= .1;
    });
    update();
  }, { root: deck, threshold: [0, .1] });
  artworks.forEach(function (art) { observer.observe(art.figure); });

  toggle.addEventListener('click', function () {
    paused = !paused;
    update();
  });
  reduced.addEventListener('change', update);
  document.addEventListener('visibilitychange', update);
})();
