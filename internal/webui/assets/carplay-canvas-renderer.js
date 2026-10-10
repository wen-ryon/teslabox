/* 自行实现的 Canvas 渲染器；帧的生命周期仍由播放器统一管理。 */
(function (root) {
  'use strict';
  // 优先使用浏览器 GPU 绘制；无 WebGL2 时仍保留非 video 的 Canvas 显示路径。
  function create(canvas) {
    const gl = canvas.getContext('webgl2', {
      alpha: false, antialias: false, depth: false, stencil: false,
      preserveDrawingBuffer: false, powerPreference: 'low-power'
    });
    if (!gl) {
      const context = canvas.getContext('2d', { alpha: false });
      if (!context) throw new Error('浏览器没有可用 Canvas 渲染上下文');
      return { mode: '2d', draw: (frame, width, height) => context.drawImage(frame, 0, 0, width, height), close() {} };
    }
    let program, buffer, texture, closed = false;
    const shaders = [];
    function release() {
      if (texture) gl.deleteTexture(texture);
      if (buffer) gl.deleteBuffer(buffer);
      if (program) gl.deleteProgram(program);
      for (const shader of shaders) gl.deleteShader(shader);
      texture = null; buffer = null; program = null; shaders.length = 0;
    }
    function shader(type, source) {
      const value = gl.createShader(type);
      if (!value) throw new Error('WebGL 无法分配着色器');
      shaders.push(value); gl.shaderSource(value, source); gl.compileShader(value);
      if (!gl.getShaderParameter(value, gl.COMPILE_STATUS)) throw new Error('WebGL 着色器编译失败');
      return value;
    }
    try {
      program = gl.createProgram();
      if (!program) throw new Error('WebGL 无法分配程序');
      gl.attachShader(program, shader(gl.VERTEX_SHADER, `#version 300 es
in vec2 position; in vec2 uv; out vec2 coordinate;
void main() { coordinate = uv; gl_Position = vec4(position, 0.0, 1.0); }`));
      gl.attachShader(program, shader(gl.FRAGMENT_SHADER, `#version 300 es
precision mediump float; in vec2 coordinate; uniform sampler2D image; out vec4 color;
void main() { color = texture(image, coordinate); }`));
      gl.linkProgram(program);
      if (!gl.getProgramParameter(program, gl.LINK_STATUS)) throw new Error('WebGL 程序链接失败');
      buffer = gl.createBuffer(); texture = gl.createTexture();
      if (!buffer || !texture) throw new Error('WebGL 资源分配失败');
      gl.useProgram(program); gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
      // VideoFrame 顶行对应纹理 v=0；顶点显式映射，避免画面上下颠倒。
      gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, 1, 0, 0, -1, -1, 0, 1, 1, 1, 1, 0, 1, -1, 1, 1]), gl.STATIC_DRAW);
      for (const [name, offset] of [['position', 0], ['uv', 8]]) {
        const location = gl.getAttribLocation(program, name);
        if (location < 0) throw new Error('WebGL 顶点属性缺失');
        gl.enableVertexAttribArray(location); gl.vertexAttribPointer(location, 2, gl.FLOAT, false, 16, offset);
      }
      gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, texture);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
      gl.uniform1i(gl.getUniformLocation(program, 'image'), 0);
      gl.pixelStorei(gl.UNPACK_FLIP_Y_WEBGL, false);
      gl.disable(gl.DEPTH_TEST); gl.disable(gl.BLEND);
    } catch (error) { release(); throw error; }
    return {
      mode: 'webgl2',
      draw(frame, width, height) {
        if (closed || gl.isContextLost()) throw new Error('WebGL 上下文已关闭或丢失');
        gl.viewport(0, 0, width, height); gl.useProgram(program);
        gl.bindBuffer(gl.ARRAY_BUFFER, buffer); gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, texture);
        gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, frame);
        gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
        if (gl.getError() !== gl.NO_ERROR) throw new Error('WebGL 视频纹理上传或绘制失败');
      },
      close() { if (!closed) { closed = true; release(); } }
    };
  }
  root.TeslaBoxRenderer = { create };
  if (typeof module === 'object' && module.exports) module.exports = root.TeslaBoxRenderer;
})(typeof globalThis !== 'undefined' ? globalThis : self);
