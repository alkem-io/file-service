import { WINSTON_MODULE_NEST_PROVIDER } from 'nest-winston';
import { NestFactory } from '@nestjs/core';
import {
  FastifyAdapter,
  NestFastifyApplication,
} from '@nestjs/platform-fastify';
import { ConfigService } from '@nestjs/config';
import helmet from '@fastify/helmet';
import fastifyCookie from '@fastify/cookie';
import cors from '@fastify/cors';
import etag from '@fastify/etag';
import { AppModule } from './app.module';
import { ConfigType } from './config';

const isProd = process.env.NODE_ENV === 'production';

(async () => {
  const app = await NestFactory.create<NestFastifyApplication>(
    AppModule,
    new FastifyAdapter({ logger: !isProd }),
    {
      /***
       * if the logger is provided at a later stage via 'useLogger' after the app has initialized, Nest falls back to the default logger
       * while initializing, which logs a lot of info logs, which we don't have control over and don't want tracked.
       * The logger is disabled while the app is loading ONLY on production to avoid the messages;
       * then the costume logger is applied as usual
       */
      logger: isProd ? false : undefined,
    },
  );
  const logger = app.get(WINSTON_MODULE_NEST_PROVIDER);
  app.useLogger(logger);

  await app.register(fastifyCookie);
  await app.register(helmet, { contentSecurityPolicy: false });
  await app.register(cors, { origin: false });
  // set the header manually, because this plugin skips generation for Stream (our case)
  // it only supports String and Buffer
  await app.register(etag);

  const configService: ConfigService<ConfigType, true> = app.get(ConfigService);
  const port = configService.get('settings.application.port', { infer: true });
  const address = configService.get('settings.application.address', {
    infer: true,
  });

  await app.listen(port, address, () => {
    logger.verbose(`File service listening on ${address}:${port}`);
  });
})();
