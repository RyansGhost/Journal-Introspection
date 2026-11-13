// Optional polish. Every page works with JavaScript off.
(function () {
  // Chart: a crosshair and tooltip that follow the pointer.
  var svg = document.querySelector('svg.chart');
  var tip = document.querySelector('.tip');
  if (svg && tip) {
    // On a narrow screen the chart scrolls sideways; start at the newest end.
    svg.parentElement.scrollLeft = svg.parentElement.scrollWidth;
    var cross = svg.querySelector('.cross');
    svg.addEventListener('pointermove', function (e) {
      var hit = e.target.closest ? e.target.closest('.hit') : null;
      if (!hit) { hide(); return; }
      var x = hit.getAttribute('data-x');
      cross.setAttribute('x1', x); cross.setAttribute('x2', x);
      cross.classList.add('show');
      tip.textContent = '';
      var head = document.createElement('strong');
      head.textContent = hit.getAttribute('data-date');
      tip.appendChild(head);
      hit.getAttribute('data-tip').split('\n').forEach(function (line) {
        var row = document.createElement('span');
        row.textContent = line;
        tip.appendChild(row);
      });
      tip.hidden = false;
      var box = svg.getBoundingClientRect();
      var px = box.width * (parseFloat(x) / svg.viewBox.baseVal.width);
      var flip = px - svg.parentElement.scrollLeft > svg.parentElement.clientWidth - 200;
      tip.style.left = (flip ? px - tip.offsetWidth - 12 : px + 12) + 'px';
    });
    svg.addEventListener('pointerleave', hide);
    function hide() { tip.hidden = true; cross.classList.remove('show'); }
  }

  // Journal: left and right arrow keys move between days.
  document.addEventListener('keydown', function (e) {
    if (e.target.matches('input, textarea') || e.metaKey || e.ctrlKey || e.altKey) return;
    var rel = e.key === 'ArrowLeft' ? 'prev' : e.key === 'ArrowRight' ? 'next' : null;
    var link = rel && document.querySelector('a[rel="' + rel + '"]');
    if (link) location.href = link.href;
  });
})();
