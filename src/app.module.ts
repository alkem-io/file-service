import { WinstonModule } from 'nest-winston';
import { Module } from '@nestjs/common';
import { ConfigModule } from '@nestjs/config';
import { WinstonConfigService } from './config';
import { AppController } from './app.controller';
import configuration from './config/configuration';
import { FileModule } from './services/file-reader';
import { BaseExceptionFilterProvider } from './core/filters';

@Module({
  imports: [
    ConfigModule.forRoot({
      envFilePath: ['.env'],
      isGlobal: true,
      load: [configuration],
    }),
    WinstonModule.forRootAsync({
      useClass: WinstonConfigService,
    }),
    FileModule,
  ],
  controllers: [AppController],
  providers: [BaseExceptionFilterProvider],
})
export class AppModule {}
